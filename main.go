/*
Fresh is a command line tool that builds and (re)starts your web application everytime you save a go or template file.

If the web framework you are using supports the Fresh runner, it will show build errors on your browser.

It currently works with Traffic (https://github.com/pilu/traffic), Martini (https://github.com/codegangsta/martini) and gocraft/web (https://github.com/gocraft/web).

Fresh will watch for file events, and every time you create/modifiy/delete a file it will build and restart the application.
If `go build` returns an error, it will logs it in the tmp folder.

Traffic (https://github.com/pilu/traffic) already has a middleware that shows the content of that file if it is present. This middleware is automatically added if you run a Traffic web app in dev mode with Fresh.
*/
package main

import (
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"strconv"
	"syscall"
	"time"
	"github.com/howeyc/fsnotify"
)

var (
	startChannel chan string
	stopChannel  chan bool
	mainLog      logFunc
	watcherLog   logFunc
	runnerLog    logFunc
	buildLog     logFunc
	appLog       logFunc
	configPath = flag.String("c", "", "config file path")
	buildOnce = flag.Bool("1", false, "build once")
)

func init() {
	startChannel = make(chan string, 1000)
	stopChannel = make(chan bool)
}

// Watches for file changes in the root directory.
// After each file system event it builds and (re)starts the application.
func main() {

	flag.Parse()

	if *configPath != "" {
		if _, err := os.Stat(*configPath); err != nil {
			fmt.Printf("Can't find config file `%s`\n", *configPath)
			os.Exit(1)
		} else {
			settings["config_path"] = *configPath
		}
	}

	//	initLimit
	{
		var rLimit syscall.Rlimit
		rLimit.Max = 10000
		rLimit.Cur = 10000
		err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rLimit)
		if err != nil {
			fmt.Println("Error Setting Rlimit ", err)
		}
	}

	// initSettings
	settings.load()

	// initLogFuncs
	mainLog = newLogFunc("main")
	watcherLog = newLogFunc("watcher")
	runnerLog = newLogFunc("runner")
	buildLog = newLogFunc("build")
	appLog = newLogFunc("app")

	// initFolders
	{
		runnerLog("InitFolders")
		path := settings.outputPath()
		runnerLog("mkdir %s", path)
		err := os.Mkdir(path, 0755)
		if err != nil {
			runnerLog(err.Error())
		}
	}

	// watch
	if !*buildOnce {
		root := settings.root()
		watch := watchDefault
		if settings["watch_method"] == "inotify" {
			watch = watchInotify
		}
		filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if info.IsDir() && !isTmpDir(path) {
				if len(path) > 1 && strings.HasPrefix(filepath.Base(path), ".") {
					return filepath.SkipDir
				}

				watch(path)
			}

			return err
		})
	}

	// start
	start()
	startChannel <- "/"

	<-make(chan int)
}

func flushEvents() {
	for {
		select {
		case eventName := <-startChannel:
			mainLog("receiving event %s", eventName)
		default:
			return
		}
	}
}

func start() {
	loopIndex := 0
	buildDelay := settings.buildDelay()

	go func() {
		for {
			loopIndex++
			mainLog("Waiting (loop %d)...", loopIndex)
			eventName := <-startChannel

			mainLog("receiving first event %s", eventName)
			mainLog("sleeping for %d milliseconds", buildDelay)
			time.Sleep(buildDelay * time.Millisecond)
			mainLog("flushing events")

			flushEvents()

			mainLog("Started! (%d Goroutines)", runtime.NumGoroutine())
			err := removeBuildErrorsLog()
			if err != nil {
				mainLog(err.Error())
			}

			if errorMessage, ok := build(); !ok {

				buildLog("Build Failed: \n %s", errorMessage)
				if loopIndex == 1 {
					os.Exit(1)
				}
				createBuildErrorsLog(errorMessage)

			} else {

				// Send shutdown signal
				shutdown()
			}

			if *buildOnce {
				os.Exit(0)
			}

			mainLog(strings.Repeat("-", 20))
		}
	}()
}

func watchInotify(path string) {

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		fatal(err)
	}

	go func() {
		for {
			select {
			case ev := <-watcher.Event:
				if isWatchedFile(ev.Name) {
					watcherLog("sending event %s", ev)
					startChannel <- ev.String()
				}
			case err := <-watcher.Error:
				watcherLog("error: %s", err)
			}
		}
	}()

	watcherLog("Watching %s", path)
	err = watcher.Watch(path)

	if err != nil {
		fatal(err)
	}
}

func watchDefault(path string) {

	getFiles := func () map[string]os.FileInfo {

		readFiles, err := ioutil.ReadDir(path)
		if err != nil {
			fatal(err)
		}

		files := make(map[string]os.FileInfo, len(readFiles))

		for _, f := range readFiles {
			files[f.Name()] = f
		}

		return files
	}

	go func () {

		files := getFiles()

		for {

			time.Sleep(500 * time.Millisecond)

			readFiles, err := ioutil.ReadDir(path)

			if os.IsNotExist(err) {
				watcherLog("found deleted directory %s", path)
				startChannel <- path
				return
			}

			if err != nil {
				fatal(err)
			}

			updateNecessary := false
			existedFiles := make(map[string]bool, len(readFiles))

			for _, f := range readFiles {

				existedFiles[f.Name()] = true

				prev, ok := files[f.Name()]
				fpath := filepath.Join(path, f.Name())

				if !ok {

					if f.IsDir() {
						watcherLog("found new directory %s", fpath)
						startChannel <- fpath
						watchDefault(fpath)
					}
					if !f.IsDir() && isWatchedFile(fpath) {
						watcherLog("found new file %s", fpath)
						startChannel <- fpath
					}

					updateNecessary = true

				} else if prev.ModTime().Unix() != f.ModTime().Unix() {

					if isWatchedFile(fpath) {
						watcherLog("found modified file %s", fpath)
						startChannel <- fpath
					}

					files[f.Name()] = f
				}
			}

			for _, f := range files {

				fpath := filepath.Join(path, f.Name())

				if !existedFiles[f.Name()] {

					if !f.IsDir() && isWatchedFile(fpath) {
						watcherLog("found deleted file %s", fpath)
						startChannel <- fpath
					}

					updateNecessary = true
				}
			}

			if (updateNecessary) {
				files = getFiles()
			}
		}
	}()

	watcherLog("Watching %s", path)
}

func build() (string, bool) {

	// Run post-build-script
	if s := settings.preBuildScript(); s.Exists() {
		buildLog("Run pre build script...")
		if errorMessage, ok := s.Run(); !ok {
			return errorMessage, false
		}
	}

	buildLog("Building...")

	arguments := []string{"build"}
	if settings.debug() {
		arguments = append(arguments, ([]string{"-gcflags", "-N -l"})...)
	}
	arguments = append(arguments, ([]string{"-o", settings.buildPath(), settings.root()})...)
	cmd := exec.Command("go", arguments...)

	buildLog("go %s", strings.Join(cmd.Args, " "))

	stderr, err := cmd.StderrPipe()
	if err != nil {
		fatal(err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fatal(err)
	}

	if path := settings.gopath(); path != "" {
		cmd.Env = append(os.Environ(), fmt.Sprintf("GOPATH=%s",path))
	}

	err = cmd.Start()
	if err != nil {
		fatal(err)
	}

	io.Copy(os.Stdout, stdout)
	errBuf, _ := ioutil.ReadAll(stderr)

	err = cmd.Wait()
	if err != nil {
		return string(errBuf), false
	}

	// Run post-build-script
	if s := settings.postBuildScript(); s.Exists() {
		buildLog("Run post build script...")
		if errorMessage, ok := s.Run(); !ok {
			return errorMessage, false
		}
	}

	return "", true
}

func shutdown() {

	if settings.shutdownSignal() == 0 {
		return
	}

	runnerLog("Send shutdown signals...")

	cmd := exec.Command("pgrep", "-f", settings.buildPath())

//	stderr, err := cmd.StderrPipe()
	_, err := cmd.StderrPipe()
	if err != nil {
		fatal(err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fatal(err)
	}

	err = cmd.Start()
	if err != nil {
		fatal(err)
	}

	outBuf, _ := ioutil.ReadAll(stdout)
//	errBuf, _ := ioutil.ReadAll(stderr)

	err = cmd.Wait()
	if err != nil {
//		return string(errBuf), false
		return
	}

	lines := strings.Split(string(outBuf), "\n")
	for _, line := range lines {
		pid, _ := strconv.ParseInt(line, 10, 32)
		if (pid > 0) {
			p, err := os.FindProcess(int(pid))
			if err == nil {
				runnerLog("Shutdown PID %d", p.Pid)
				p.Signal(settings.shutdownSignal())
			}
		}
	}
}
