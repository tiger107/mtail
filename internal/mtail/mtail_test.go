// Copyright 2011 Google Inc. All Rights Reserved.
// This file is available under the Apache license.

package mtail

import (
	"expvar"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang/glog"
	"github.com/google/mtail/internal/metrics"
	"github.com/google/mtail/internal/testutil"
	"github.com/google/mtail/internal/watcher"
)

const testProgram = "/$/ { }\n"

func makeTempDir(t *testing.T) (workdir string) {
	var err error
	if workdir, err = ioutil.TempDir("", "mtail_test"); err != nil {
		t.Fatalf("ioutil.TempDir failed: %s", err)
	}
	return
}

func removeTempDir(t *testing.T, workdir string) {
	if err := os.RemoveAll(workdir); err != nil {
		t.Fatalf("os.RemoveAll failed: %s", err)
	}
}

func startMtailServer(t *testing.T, options ...func(*Server) error) *Server {
	expvar.Get("line_count").(*expvar.Int).Set(0)
	expvar.Get("log_count").(*expvar.Int).Set(0)
	expvar.Get("log_rotations_total").(*expvar.Map).Init()
	expvar.Get("prog_loads_total").(*expvar.Map).Init()

	w, err := watcher.NewLogWatcher(0, true)
	if err != nil {
		t.Errorf("Couodn't make a log watcher: %s", err)
	}
	m, err := New(metrics.NewStore(), w, options...)
	if err != nil {
		t.Fatalf("couldn't create mtail: %s", err)
	}
	if pErr := m.l.CompileAndRun("test", strings.NewReader(testProgram)); pErr != nil {
		t.Errorf("Couldn't compile program: %s", pErr)
	}

	if err := m.StartTailing(); err != nil {
		t.Errorf("StartTailing failed: %s", err)
	}
	return m
}
func TestHandleLogUpdates(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}
	workdir := makeTempDir(t)
	defer removeTempDir(t, workdir)
	// touch log file
	logFilepath := path.Join(workdir, "log")
	logFile, err := os.Create(logFilepath)
	if err != nil {
		t.Errorf("could not touch log file: %s", err)
	}
	defer logFile.Close()
	m := startMtailServer(t, LogPathPatterns(logFilepath))
	defer m.Close()
	inputLines := []string{"hi", "hi2", "hi3"}
	for i, x := range inputLines {
		// write to log file
		testutil.WriteString(t, logFile, x+"\n")
		// check log line count increase
		expected := fmt.Sprintf("%d", i+1)
		check := func() (bool, error) {
			if expvar.Get("line_count").String() != expected {
				return false, nil
			}
			return true, nil
		}
		ok, err := testutil.DoOrTimeout(check, 100*time.Millisecond, 10*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Errorf("Line count not increased\n\texpected: %s\n\treceived: %s", expected, expvar.Get("line_count").String())
			buf := make([]byte, 1<<16)
			count := runtime.Stack(buf, true)
			fmt.Println(string(buf[:count]))
		}
	}
}

func TestHandleLogRotation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}
	workdir := makeTempDir(t)
	defer removeTempDir(t, workdir)
	logFilepath := path.Join(workdir, "log")
	// touch log file
	logFile, err := os.Create(logFilepath)
	if err != nil {
		t.Errorf("could not touch log file: %s", err)
	}
	// Create a logger
	hup := make(chan bool, 1)
	m := startMtailServer(t, LogPathPatterns(logFilepath))
	defer func() {
		if cerr := m.Close(); cerr != nil {
			t.Fatal(cerr)
		}
	}()

	// Rotate the log file after 500ms.
	go func() {
		<-time.After(5 * 100 * time.Millisecond)
		err = os.Rename(logFilepath, logFilepath+".1")
		if err != nil {
			t.Errorf("could not rename log file: %s", err)
		}
		// "sighup" the "logging process"
		hup <- true
	}()
	i := 0
Loop:
	for {
		select {
		case <-hup:
			// Close to flush contents
			if err = logFile.Close(); err != nil {
				t.Fatal(err)
			}
			// Received a HUP so reopen the logfile.
			logFile, err = os.OpenFile(logFilepath, os.O_RDWR|os.O_CREATE, 0)
			if err != nil {
				t.Errorf("could not create rotated log file: %s", err)
			}
			err = logFile.Chmod(0666)
			if err != nil {
				t.Errorf("could not chmod log file to read: %s", err)
			}
		case <-time.Tick(100 * time.Millisecond):
			if i >= 10 {
				break Loop
			}
			testutil.WriteString(t, logFile, fmt.Sprintf("%d\n", i))
			i++
		}
	}
	// Close to flush contents.
	if err = logFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	expected := "10"
	if diff := testutil.Diff(expected, expvar.Get("line_count").String()); diff != "" {
		t.Errorf("line_count metric didn't match\n%s", diff)
	}
	rotationsMap := expvar.Get("log_rotations_total").(*expvar.Map)
	v := rotationsMap.Get(logFilepath)
	if v == nil {
		t.Errorf("path %q not found in map: %v", logFilepath, rotationsMap)
	}
	diff := testutil.Diff("1", v.String())
	if diff != "" {
		t.Errorf("log_rotations_total metric didn't match\n%s", diff)
	}
}

func TestHandleNewLogAfterStart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	workdir := makeTempDir(t)
	defer removeTempDir(t, workdir)
	// Start up mtail
	logFilepath := path.Join(workdir, "log")
	m := startMtailServer(t, LogPathPatterns(logFilepath))
	defer m.Close()
	time.Sleep(10 * time.Millisecond)

	// touch log file
	logFile, err := os.Create(logFilepath)
	if err != nil {
		t.Errorf("could not touch log file: %s", err)
	}
	defer logFile.Close()
	testutil.WriteString(t, logFile, "a\n")

	expectedLogCount := "1"
	expectedLineCount := "1"
	check := func() (bool, error) {
		if expvar.Get("log_count").String() != expectedLogCount {
			return false, nil
		}
		if expvar.Get("line_count").String() != expectedLineCount {
			return false, nil
		}
		return true, nil
	}
	ok, err := testutil.DoOrTimeout(check, 100*time.Millisecond, 10*time.Millisecond)
	if err != nil {
		t.Error(err)
	}
	if !ok {
		t.Errorf("Log count\n\texpected: %s\n\treceived: %s", expectedLogCount, expvar.Get("log_count").String())
		t.Errorf("Line count\n\texpected: %s\n\treceived: %s", expectedLineCount, expvar.Get("line_count").String())
	}
}

func TestHandleNewLogIgnored(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}
	workdir := makeTempDir(t)
	defer removeTempDir(t, workdir)
	// Start mtail
	logFilepath := path.Join(workdir, "log")
	m := startMtailServer(t, LogPathPatterns(logFilepath))
	defer m.Close()

	// touch log file
	newLogFilepath := path.Join(workdir, "log1")

	logFile, err := os.Create(newLogFilepath)
	if err != nil {
		t.Errorf("could not open log file: %s", err)
	}
	defer logFile.Close()
	expected := "0"
	if expvar.Get("log_count").String() != expected {
		t.Errorf("Log count not increased\n\texpected: %s\n\treceived: %s", expected, expvar.Get("log_count").String())
	}
}

func TestHandleSoftLinkChange(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}
	workdir := makeTempDir(t)
	defer removeTempDir(t, workdir)

	logFilepath := path.Join(workdir, "log")
	m := startMtailServer(t, LogPathPatterns(logFilepath))
	defer m.Close()

	trueLog1, err := os.Create(logFilepath + ".true1")
	if err != nil {
		t.Errorf("could not create log file: %s", err)
	}
	defer trueLog1.Close()
	err = os.Symlink(logFilepath+".true1", logFilepath)
	if err != nil {
		t.Errorf("could not create symlink: %s", err)
	}
	inputLines := []string{"hi1", "hi2", "hi3"}
	for _, x := range inputLines {
		testutil.WriteString(t, trueLog1, x+"\n")
		testutil.FatalIfErr(t, trueLog1.Sync())
	}
	check3 := func() (bool, error) {
		if expvar.Get("log_count").String() != "1" {
			return false, nil
		}
		if expvar.Get("log_rotations_total").(*expvar.Map).Get(logFilepath) != nil {
			return false, nil
		}
		return true, nil
	}
	ok, err := testutil.DoOrTimeout(check3, 1*time.Second, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Errorf("log count: received %s, expected 1", expvar.Get("log_count").String())
		t.Errorf("log rotatins: received %s, expected 0", expvar.Get("log_rotations_total").String())
	}
	trueLog2, err := os.Create(logFilepath + ".true2")
	if err != nil {
		t.Errorf("could not create log file: %s", err)
	}
	defer trueLog2.Close()
	err = os.Remove(logFilepath)
	if err != nil {
		t.Errorf("could not delete symlink: %s", err)
	}
	err = os.Symlink(logFilepath+".true2", logFilepath)
	if err != nil {
		t.Errorf("could not create symlink: %s", err)
	}
	for _, x := range inputLines {
		testutil.WriteString(t, trueLog2, x+"\n")
		testutil.FatalIfErr(t, trueLog2.Sync())
	}
	check6 := func() (bool, error) {
		if expvar.Get("log_count").String() != "1" {
			return false, nil
		}
		if expvar.Get("log_rotations_total").(*expvar.Map).Get(logFilepath) == nil {
			return false, nil
		}
		if expvar.Get("log_rotations_total").(*expvar.Map).Get(logFilepath).String() != "1" {
			return false, nil
		}
		return true, nil
	}
	ok, err = testutil.DoOrTimeout(check6, 100*time.Millisecond, 10*time.Millisecond)
	if err != nil {
		buf := make([]byte, 1<<16)
		count := runtime.Stack(buf, true)
		t.Log("Timed out: Dumping goroutine stack")
		t.Log(string(buf[:count]))
		t.Fatal(err)
	}
	if !ok {
		t.Errorf("log count: received %s, expected 1", expvar.Get("log_count").String())
		t.Errorf("log rotatins: received %s, expected 0", expvar.Get("log_rotations_total").String())
	}
	_, err = os.Stat(logFilepath + ".true1")
	if err != nil {
		t.Errorf("stat failed on %s: %s", logFilepath+".true1", err)
	}
	_, err = os.Stat(logFilepath + ".true2")
	if err != nil {
		t.Errorf("stat failed on %s: %s", logFilepath+".true2", err)
	}
}

func TestGlob(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	workdir := makeTempDir(t)
	defer removeTempDir(t, workdir)

	globTests := []struct {
		name     string
		expected bool
	}{
		{
			path.Join(workdir, "log1"),
			true,
		},
		{
			path.Join(workdir, "log2"),
			true,
		},
		{
			path.Join(workdir, "1log"),
			false,
		},
	}
	count := 0
	for _, tt := range globTests {
		log, err := os.Create(tt.name)
		if err != nil {
			t.Errorf("could not create log file: %s", err)
			continue
		}
		defer log.Close()
		if tt.expected {
			count++
		}
		testutil.WriteString(t, log, "\n")
		testutil.FatalIfErr(t, err)
		testutil.FatalIfErr(t, log.Sync())
	}
	m := startMtailServer(t, LogPathPatterns(path.Join(workdir, "log*")))
	defer m.Close()
	check := func() (bool, error) {
		if expvar.Get("log_count").String() != fmt.Sprintf("%d", count) {
			glog.V(1).Infof("tailer is %q, count is %d", expvar.Get("log_count").String(), count)
			return false, nil
		}
		return true, nil
	}
	ok, err := testutil.DoOrTimeout(check, 10*time.Second, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Errorf("Log count not matching\n\texpected: %d\n\t: received: %s", count, expvar.Get("log_count").String())
	}
}

func TestGlobAfterStart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	workdir := makeTempDir(t)
	defer removeTempDir(t, workdir)

	globTests := []struct {
		name     string
		expected bool
	}{
		{
			path.Join(workdir, "log1"),
			true,
		},
		{
			path.Join(workdir, "log2"),
			true,
		},
		{
			path.Join(workdir, "1log"),
			false,
		},
	}
	m := startMtailServer(t, LogPathPatterns(path.Join(workdir, "log*")))
	defer m.Close()
	glog.Infof("Pausing for mtail startup.")
	time.Sleep(100 * time.Millisecond)
	count := 0
	for _, tt := range globTests {
		log, err := os.Create(tt.name)
		if err != nil {
			t.Errorf("could not create log file: %s", err)
			continue
		}
		defer log.Close()
		if tt.expected {
			count++
		}
		testutil.WriteString(t, log, "\n")
		testutil.FatalIfErr(t, log.Sync())
	}
	glog.Infof("count is %d", count)
	check := func() (bool, error) {
		if expvar.Get("log_count").String() != fmt.Sprintf("%d", count) {
			glog.V(1).Infof("tailer is %q, count is %d", expvar.Get("log_count").String(), count)
			return false, nil
		}
		return true, nil
	}
	ok, err := testutil.DoOrTimeout(check, 10*time.Second, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Errorf("Log count not matching\n\texpected: %d\n\t: received: %s", count, expvar.Get("log_count").String())
	}
}

// func TestHandleLogDeletes(t *testing.T) {
// 	if testing.Short() {
// 		t.Skip("skipping test in short mode")
// 	}
// 	workdir := makeTempDir(t)
// 	defer removeTempDir(t, workdir)
// 	// touch log file
// 	logFilepath := path.Join(workdir, "log")
// 	logFile, err := os.Create(logFilepath)
// 	if err != nil {
// 		t.Errorf("could not touch log file: %s", err)
// 	}
// 	defer logFile.Close()
// 	m := startMtailServer(t, LogPathPatterns(logFilepath))
// 	defer m.Close()

// 	if err = os.Remove(logFilepath); err != nil {
// 		t.Fatal(err)
// 	}

// 	expected := "0"
// 	check := func() (bool, error) {
// 		if expvar.Get("log_count").String() != expected {
// 			return false, nil
// 		}
// 		return true, nil
// 	}
// 	ok, err := testutil.DoOrTimeout(check, 100*time.Millisecond, 10*time.Millisecond)
// 	if err != nil {
// 		buf := make([]byte, 1<<16)
// 		count := runtime.Stack(buf, true)
// 		t.Log("Timed out: Dumping goroutine stack")
// 		t.Log(string(buf[:count]))
// 		t.Fatal(err)
// 	}
// 	if !ok {
// 		t.Errorf("Log count not decreased\n\texpected: %s\n\treceived %s", expected, expvar.Get("log_count").String())
// 	}
// }

func TestHandleLogTruncate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}
	workdir := makeTempDir(t)
	defer removeTempDir(t, workdir)

	logFilepath := path.Join(workdir, "log")
	logFile, err := os.Create(logFilepath)
	if err != nil {
		t.Errorf("could not touch log file: %s", err)
	}
	defer logFile.Close()
	m := startMtailServer(t, LogPathPatterns(logFilepath))
	defer func() {
		if cerr := m.Close(); cerr != nil {
			t.Fatal(cerr)
		}
	}()

	testutil.WriteString(t, logFile, "x\n")
	glog.Info("Write")
	check := func() (bool, error) {
		if expvar.Get("line_count").String() != "1" {
			return false, nil
		}
		return true, nil
	}
	ok, err := testutil.DoOrTimeout(check, 10*time.Second, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Errorf("log line count received %s, expected 1", expvar.Get("log_count").String())
	}
	testutil.FatalIfErr(t, logFile.Truncate(0))
	glog.Infof("Truncate")
	testutil.WriteString(t, logFile, "x\n")
	glog.Info("Write")
	check2 := func() (bool, error) {
		if expvar.Get("line_count").String() != "2" {
			return false, nil
		}
		return true, nil
	}
	ok, err = testutil.DoOrTimeout(check2, 10*time.Second, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Errorf("log line count received %s, expected 2", expvar.Get("log_count").String())
	}
}

func TestHandleRelativeLogAppend(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}
	workdir := makeTempDir(t)
	defer removeTempDir(t, workdir)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	glog.Infof("cwd is %q", cwd)

	if cerr := os.Chdir(workdir); cerr != nil {
		t.Fatal(cerr)
	}
	defer func() {
		if cerr := os.Chdir(cwd); err != nil {
			t.Error(cerr)
		}
	}()

	// touch log file
	logFilepath := path.Join(workdir, "log")
	logFile, err := os.Create(logFilepath)
	if err != nil {
		t.Errorf("could not touch log file: %s", err)
	}
	defer logFile.Close()
	pathnames := []string{"log"}
	m := startMtailServer(t, LogPathPatterns(pathnames...))
	defer m.Close()
	inputLines := []string{"hi", "hi2", "hi3"}
	for i, x := range inputLines {
		// write to log file
		testutil.WriteString(t, logFile, x+"\n")
		// check log line count increase
		expected := fmt.Sprintf("%d", i+1)
		check := func() (bool, error) {
			if expvar.Get("line_count").String() != expected {
				return false, nil
			}
			return true, nil
		}
		ok, err := testutil.DoOrTimeout(check, 100*time.Millisecond, 10*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Errorf("Line count not increased\n\texpected: %s\n\treceived: %s", expected, expvar.Get("line_count").String())
			buf := make([]byte, 1<<16)
			count := runtime.Stack(buf, true)
			fmt.Println(string(buf[:count]))
		}
	}

}

func TestProgramReloadNoDuplicateMetrics(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in shor tmode")
	}

	workdir := makeTempDir(t)
	defer removeTempDir(t, workdir)

	logDir := path.Join(workdir, "logs")
	if err := os.Mkdir(logDir, 0777); err != nil {
		t.Fatal(err)
	}
	progDir := path.Join(workdir, "progs")
	if err := os.Mkdir(progDir, 0777); err != nil {
		t.Fatal(err)
	}

	logFilepath := path.Join(logDir, "log")
	logFile, err := os.Create(logFilepath)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()

	m := startMtailServer(t, ProgramPath(progDir), LogPathPatterns(logDir+"/*"))
	defer m.Close()
	store := m.store

	v := expvar.Get("prog_loads_total").(*expvar.Map).Get("program.mtail")
	if v != nil {
		t.Log(v)
	}

	progpath := path.Join(progDir, "program.mtail")
	p, err := os.Create(progpath)
	if err != nil {
		t.Fatalf("couldn't open program file: %s", err)
	}
	testutil.WriteString(t, p, "counter foo\n/^foo$/ {\n foo++\n }\n")
	testutil.FatalIfErr(t, p.Close())

	check := func() (bool, error) {
		v := expvar.Get("prog_loads_total").(*expvar.Map).Get("program.mtail")
		if v == nil {
			return false, nil
		}
		n, nerr := strconv.Atoi(v.String())
		if nerr != nil {
			return false, nerr
		}
		if n < 1 {
			return false, nil
		}
		return true, nil
	}
	ok, err := testutil.DoOrTimeout(check, time.Second, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("program loads didn't increase")
	}
	mfoo := store.Metrics["foo"]
	if len(mfoo) != 1 || len(mfoo[0].LabelValues) != 1 {
		t.Errorf("Unexpected metrics content: expected a single metric with no labels, but got all this %v", mfoo)
	}

	n, err := logFile.WriteString("foo\n")
	if err != nil {
		t.Fatal(err)
	}
	if n < 4 {
		t.Fatalf("only wrote %d", n)
	}
	time.Sleep(100 * time.Millisecond)

	checkFoo := func() (bool, error) {
		v := store.Metrics["foo"][0]
		d, derr := v.GetDatum()
		if derr != nil {
			return false, derr
		}
		if d.ValueString() != "1" {
			t.Log(d)
			return false, nil
		}
		return true, nil
	}
	ok, err = testutil.DoOrTimeout(checkFoo, time.Second, 10*time.Millisecond)
	if err != nil {
		t.Error(err)
	}
	if !ok {
		t.Fatal("foo didn't increase")
	}

	p, err = os.Create(progpath)
	if err != nil {
		t.Fatalf("couldn't open program file: %s", err)
	}
	testutil.WriteString(t, p, "counter foo\n/^foo$/ {\n foo++\n }\n")
	testutil.FatalIfErr(t, p.Close())

	check2 := func() (bool, error) {
		v := expvar.Get("prog_loads_total")
		v = v.(*expvar.Map).Get("program.mtail")
		if v == nil {
			return false, nil
		}
		n, nerr := strconv.Atoi(v.String())
		if nerr != nil {
			return false, nerr
		}
		if n < 2 {
			return false, nil
		}
		return true, nil
	}
	ok, err = testutil.DoOrTimeout(check2, time.Second, 10*time.Millisecond)
	if err != nil {
		t.Error(err)
	}
	if !ok {
		t.Error("program loads didn't increase")
	}
	store.Lock()
	mfoo = store.Metrics["foo"]
	if len(mfoo) != 1 || len(mfoo[0].LabelValues) != 1 {
		t.Errorf("Unexpected metrics content: expected a single metric with no labels, but got all this: %v", mfoo)
	}
	store.Unlock()
}

func TestBuildInfo(t *testing.T) {
	buildInfo := BuildInfo{
		Branch:   "foo",
		Version:  "bar",
		Revision: "baz",
	}

	buildInfoWant := fmt.Sprintf(
		"mtail version bar git revision baz go version %s go arch %s go os %s",
		runtime.Version(),
		runtime.GOARCH,
		runtime.GOOS,
	)
	buildInfoGot := buildInfo.String()

	if buildInfoWant != buildInfoGot {
		t.Errorf("Unexpected build info string, want: %q, got: %q", buildInfoWant, buildInfoGot)
	}
}

func TestFilenameRegexIgnore(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	workdir := makeTempDir(t)
	defer removeTempDir(t, workdir)

	globTests := []struct {
		name     string
		expected bool
	}{
		{
			path.Join(workdir, "log1"),
			true,
		},
		{
			path.Join(workdir, "log1.gz"),
			false,
		},
		{
			path.Join(workdir, "log2gz"),
			true,
		},
	}
	count := 0
	for _, tt := range globTests {
		log, err := os.Create(tt.name)
		if err != nil {
			t.Errorf("could not create log file: %s", err)
			continue
		}
		defer log.Close()
		if tt.expected {
			count++
		}
		testutil.WriteString(t, log, "\n")
		testutil.FatalIfErr(t, err)
		testutil.FatalIfErr(t, log.Sync())
	}
	m := startMtailServer(t, LogPathPatterns(path.Join(workdir, "log*")), IgnoreRegexPattern("\\.gz"))
	defer m.Close()
	check := func() (bool, error) {
		if expvar.Get("log_count").String() != fmt.Sprintf("%d", count) {
			glog.V(1).Infof("tailer is %q, count is %d", expvar.Get("log_count").String(), count)
			return false, nil
		}
		return true, nil
	}
	ok, err := testutil.DoOrTimeout(check, 10*time.Second, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Errorf("Log count not matching\n\texpected: %d\n\t: received: %s", count, expvar.Get("log_count").String())
	}
}

func TestIgnoreFolder(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	workdir := makeTempDir(t)
	defer removeTempDir(t, workdir)

	globTests := []struct {
		name     string
		isFolder bool
		expected bool
	}{
		{
			path.Join(workdir, "log1"),
			false,
			true,
		},
		{
			path.Join(workdir, "logarchive"),
			true,
			false,
		},
		{
			path.Join(workdir, "log2.gz"),
			false,
			false,
		},
	}
	count := 0
	for _, tt := range globTests {
		var err error
		var log *os.File

		if tt.isFolder {
			err = os.Mkdir(tt.name, 0700)
			testutil.FatalIfErr(t, err)
			continue
		} else {
			log, err = os.Create(tt.name)
		}

		if !tt.isFolder && tt.expected {
			count++
		}
		defer log.Close()
		testutil.FatalIfErr(t, err)
		testutil.WriteString(t, log, "\n")
		testutil.FatalIfErr(t, log.Sync())
	}
	m := startMtailServer(t, LogPathPatterns(path.Join(workdir, "log*")), IgnoreRegexPattern("\\.gz"))
	defer m.Close()
	check := func() (bool, error) {
		if expvar.Get("log_count").String() != fmt.Sprintf("%d", count) {
			glog.V(1).Infof("tailer is %q, count is %d", expvar.Get("log_count").String(), count)
			return false, nil
		}
		return true, nil
	}
	ok, err := testutil.DoOrTimeout(check, 10*time.Second, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Errorf("Log count not matching\n\texpected: %d\n\t: received: %s", count, expvar.Get("log_count").String())
	}
}
