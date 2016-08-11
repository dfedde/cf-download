/*
* IBM jStart team cf download cli Plugin
* A plugin for downloading contents of a running app's file directory
*
* Authors: Miguel Clement, Jake Eden
* Date: 3/5/2015
*
* for cross platform compiling use gox (https://github.com/mitchellh/gox)
* gox compile command: gox -output="binaries/{{.OS}}/{{.Arch}}/cf-download" -osarch="linux/amd64 darwin/amd64 windows/amd64"
 */

package main

import (
	"flag"
	"fmt"
	"github.com/cloudfoundry/cli/plugin"
	"github.com/ibmjstart/cf-download/cmd_exec"
	"github.com/ibmjstart/cf-download/dir_parser"
	"github.com/ibmjstart/cf-download/downloader"
	"github.com/ibmjstart/cf-download/filter"
	"github.com/mgutz/ansi"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

/*
*	This is the struct implementing the interface defined by the core CLI. It can
*	be found at  "github.com/cloudfoundry/cli/plugin/plugin.go"
 */
type DownloadPlugin struct{}

// contains flag values
type flagVal struct {
	Omit_flag      string
	OverWrite_flag bool
	Instance_flag  string
	Verbose_flag   bool
	File_flag      bool
}

// contains local and server paths
type pathVal struct {
	RootWorkingDirectoryLocal  string
	RootWorkingDirectoryServer string
	StartingPathServer         string
}

var (
	rootWorkingDirectoryServer string
	appName                    string
	filesDownloaded            int
	failedDownloads            []string
	parser                     dir_parser.Parser
	dloader                    downloader.Downloader
)

// global wait group for all download threads
var wg sync.WaitGroup

/*
*	This function must be implemented by any plugin because it is part of the
*	plugin interface defined by the core CLI.
*
*	Run(....) is the entry point when the core CLI is invoking a command defined
*	by a plugin. The first parameter, plugin.CliConnection, is a struct that can
*	be used to invoke cli commands. The second paramter, args, is a slice of
*	strings. args[0] will be the name of the command, and will be followed by
*	any additional arguments a cli user typed in.
*
*	Any error handling should be handled with the plugin itself (this means printing
*	user facing errors). The CLI will exit 0 if the plugin exits 0 and will exit
*	1 should the plugin exits nonzero.
 */

func (c *DownloadPlugin) Run(cliConnection plugin.CliConnection, args []string) {
	if args[0] != "download" {
		os.Exit(0)
	}

	// start time for download timer
	start := time.Now()

	// disables ansi text color on windows
	onWindows := IsWindows()

	if len(args) < 2 {
		fmt.Println(createMessage("\nError: Missing App Name", "red+b", onWindows))
		printHelp()
		os.Exit(1)
	}

	flagVals, paths := ParseArgs(args)

	// flag variables
	filterList := filter.GetFilterList(flagVals.Omit_flag, flagVals.Verbose_flag) // get list of things to not download

	workingDir, err := os.Getwd()
	check(err, "Called by: Getwd")
	pathVals := GetDirectoryContext(workingDir, paths, flagVals.File_flag)
	for _, v := range pathVals {
		v.RootWorkingDirectoryLocal = filepath.FromSlash(v.RootWorkingDirectoryLocal)
	}

	// ensure cf_trace is disabled, otherwise parsing breaks
	if os.Getenv("CF_TRACE") == "true" {
		fmt.Println("\nError: environment variable CF_TRACE is set to true. This prevents download from succeeding.")
		return
	}

	for _, v := range pathVals {
		// prevent overwriting files
		if Exists(v.RootWorkingDirectoryLocal) && flagVals.OverWrite_flag == false {
			fmt.Println("\nError: destination path", v.RootWorkingDirectoryLocal, "already exists.\n\nDelete it or rerun the command with the '--overwrite' flag.")
			os.Exit(1)
		}

		// remove files to be overwritten
		if flagVals.OverWrite_flag {
			err := os.RemoveAll(v.RootWorkingDirectoryLocal)
			check(err, "Cannot remove "+v.RootWorkingDirectoryLocal+" for overwrite.")
		}

		cmdExec := cmd_exec.NewCmdExec()
		parser = dir_parser.NewParser(cmdExec, appName, flagVals.Instance_flag, onWindows, flagVals.Verbose_flag)
		dloader = downloader.NewDownloader(cmdExec, &wg, appName, flagVals.Instance_flag, v.RootWorkingDirectoryServer, flagVals.Verbose_flag, onWindows)

		// stop consoleWriter
		quit := make(chan int)

		// disable consoleWriter if verbose
		if flagVals.Verbose_flag == false {
			go consoleWriter(quit)
		}

		if flagVals.File_flag {
			err := os.MkdirAll(strings.TrimSuffix(v.RootWorkingDirectoryLocal, filepath.Base(v.RootWorkingDirectoryLocal)), 0755)
			check(err, "Error D1: failed to create directory.")

			wg.Add(1)
			dloader.DownloadFile(v.StartingPathServer, v.RootWorkingDirectoryLocal, &wg)
		} else {
			// parse the directory
			files, dirs := parser.ExecParseDir(v.StartingPathServer)

			// Start the download
			wg.Add(1)
			dloader.Download(files, dirs, v.StartingPathServer, v.RootWorkingDirectoryLocal, filterList)
		}

		// Wait for download goRoutines
		wg.Wait()

		// stop console writer
		if flagVals.Verbose_flag == false {
			quit <- 0
		}
	}

	getFailedDownloads()
	PrintCompletionInfo(start, onWindows)
}

/*
*	-----------------------------------------------------------------------------------------------
* 	------------------------------------- Helper Functions ----------------------------------------
* 	-----------------------------------------------------------------------------------------------
 */

func getFailedDownloads() {
	failedDownloads = append(parser.GetFailedDownloads(), dloader.GetFailedDownloads()...)
}

func GetDirectoryContext(workingDir string, paths []string, isFile bool) []pathVal {
	var pathVals []pathVal

	rootWorkingDir := workingDir + "/"
	localPath := rootWorkingDir
	startingPath := "/"

	if len(paths) == 0 {
		// create appName directory if downloading whole app
		addPathVals := pathVal{
			RootWorkingDirectoryLocal:  localPath + appName + "/",
			RootWorkingDirectoryServer: rootWorkingDir + appName + "/",
			StartingPathServer:         startingPath,
		}

		pathVals = append(pathVals, addPathVals)
	} else {
		// append each path provided as an argument
		for _, v := range paths {
			if !strings.HasSuffix(v, "/") && !isFile {
				v += "/"
			}
			if strings.HasPrefix(v, "/") {
				v = strings.TrimPrefix(v, "/")
			}

			addPathVals := pathVal{
				RootWorkingDirectoryLocal:  localPath + filepath.Base(v),
				RootWorkingDirectoryServer: rootWorkingDir + v,
				StartingPathServer:         startingPath + v,
			}

			if !isFile {
				addPathVals.RootWorkingDirectoryLocal += "/"
			}

			pathVals = append(pathVals, addPathVals)
		}
	}

	return pathVals
}

func ParseArgs(args []string) (flagVal, []string) {
	// Create flagSet f1
	f1 := flag.NewFlagSet("f1", flag.ContinueOnError)

	// Create flags
	omitp := f1.String("omit", "", "--omit path/to/some/file")
	overWritep := f1.Bool("overwrite", false, "--overwrite")
	instancep := f1.Int("i", 0, "-i [instanceNum]")
	verbosep := f1.Bool("verbose", false, "--verbose")
	filep := f1.Bool("file", false, "--file")

	// Get paths
	var paths []string
	for i, v := range args {
		if strings.HasPrefix(v, "-") {
			break
		} else if i > 1 {
			paths = append(paths, v)
		}
	}

	err := f1.Parse(args[(2 + len(paths)):])

	// check for misplaced flags
	appName = args[1]
	if strings.HasPrefix(appName, "-") || strings.HasPrefix(appName, "--") {
		fmt.Println(createMessage("\nError: App name begins with '-' or '--'. correct flag usage: 'cf download APP_NAME [--flags]'", "red+b", IsWindows()))
		printHelp()
		os.Exit(1)
	}

	// Check for parsing errors, display usage
	if err != nil {
		fmt.Println("\nError: ", err, "\n")
		printHelp()
		os.Exit(1)
	}

	flagVals := flagVal{
		Omit_flag:      string(*omitp),
		OverWrite_flag: bool(*overWritep),
		Instance_flag:  strconv.Itoa(*instancep),
		Verbose_flag:   *verbosep,
		File_flag:      *filep,
	}

	return flagVals, paths
}

/*
*	consoleWriter prints the current number of files downloaded. It is polled every 350 milleseconds
* 	disabled if using verbose flag.
 */
func consoleWriter(quit chan int) {
	count := 0
	for {
		filesDownloaded := dloader.GetFilesDownloadedCount()
		select {
		case <-quit:
			fmt.Println("\rFiles downloaded:", filesDownloaded, "  ")
			return
		default:
			switch count = (count + 1) % 4; count {
			case 0:
				fmt.Printf("\rFiles downloaded: %d \\ ", filesDownloaded)
			case 1:
				fmt.Printf("\rFiles downloaded: %d | ", filesDownloaded)
			case 2:
				fmt.Printf("\rFiles downloaded: %d / ", filesDownloaded)
			case 3:
				fmt.Printf("\rFiles downloaded: %d --", filesDownloaded)
			}
			time.Sleep(350 * time.Millisecond)
		}
	}
}

// prints all the info you see at program finish
func PrintCompletionInfo(start time.Time, onWindows bool) {
	// let user know if any files were inaccessible
	if len(failedDownloads) == 1 {
		fmt.Println("")
		fmt.Println(len(failedDownloads), "file or directory was not downloaded (permissions issue or corrupt):")
		PrintSlice(failedDownloads)
	} else if len(failedDownloads) > 1 {
		fmt.Println("")
		fmt.Println(len(failedDownloads), "files or directories were not downloaded (permissions issue or corrupt):")
		PrintSlice(failedDownloads)
	}

	if len(failedDownloads) > 100 {
		fmt.Println("\nYou had over 100 failed downloads, we highly recommend you omit the failed file's open parent directories using the omit flag.\n")
	}

	// display runtime
	elapsed := time.Since(start)
	elapsedString := strings.Split(elapsed.String(), ".")[0]
	elapsedString = strings.TrimSuffix(elapsedString, ".") + "s"
	fmt.Println("\nDownload time: " + elapsedString)

	msg := ansi.Color(appName+" Successfully Downloaded!", "green+b")
	if onWindows == true {
		msg = "Successfully Downloaded!"
	}
	fmt.Println(msg)
}

// error check function
func check(e error, errMsg string) {
	if e != nil {
		fmt.Println("\nError: ", e)
		if errMsg != "" {
			fmt.Println("Message: ", errMsg)
		}
		os.Exit(1)
	}
}

// prints slices in readable format
func PrintSlice(slice []string) error {
	for index, val := range slice {
		fmt.Println(index+1, ": ", val)
	}
	return nil
}

func IsWindows() bool {
	return runtime.GOOS == "windows"
}

// Exists returns whether the given file or directory Exists or not
func Exists(path string) bool {
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	check(err, "Error E0.")
	return false
}

func createMessage(message, color string, onWindows bool) string {
	errmsg := ansi.Color(message, color)
	if onWindows == true {
		errmsg = message
	}

	return errmsg
}

func printHelp() {
	cmd := exec.Command("cf", "help", "download")
	output, _ := cmd.CombinedOutput()
	fmt.Printf("%s", output)
}

/*
*	This function must be implemented as part of the	plugin interface
*	defined by the core CLI.
*
*	GetMetadata() returns a PluginMetadata struct. The first field, Name,
*	determines the name of the plugin which should generally be without spaces.
*	If there are spaces in the name a user will need to properly quote the name
*	during uninstall otherwise the name will be treated as seperate arguments.
*	The second value is a slice of Command structs. Our slice only contains one
*	Command Struct, but could contain any number of them. The first field Name
*	defines the command `cf basic-plugin-command` once installed into the CLI. The
*	second field, HelpText, is used by the core CLI to display help information
*	to the user in the core commands `cf help`, `cf`, or `cf -h`.
 */
func (c *DownloadPlugin) GetMetadata() plugin.PluginMetadata {
	return plugin.PluginMetadata{
		Name: "cf-download",
		Version: plugin.VersionType{
			Major: 1,
			Minor: 1,
			Build: 0,
		},
		Commands: []plugin.Command{
			plugin.Command{
				Name:     "download",
				HelpText: "Download contents of a running app's file directory",

				// UsageDetails is optional
				// It is used to show help of usage of each command
				UsageDetails: plugin.Usage{
					Usage: "cf download APP_NAME [PATH...] [--overwrite] [--file] [--verbose] [--omit ommited_paths] [-i instance_num]",
					Options: map[string]string{
						"-overwrite":             "Overwrite existing files",
						"-file":                  "Specify a file",
						"-verbose":               "Verbose output",
						"-omit \"path/to/file\"": "Omit directories or files (delimited by semicolons)",
						"i": "Instance",
					},
				},
			},
		},
	}
}

/*
* Unlike most Go programs, the `Main()` function will not be used to run all of the
* commands provided in your plugin. Main will be used to initialize the plugin
* process, as well as any dependencies you might require for your
* plugin.
 */
func main() {

	// Any initialization for your plugin can be handled here

	// Note: The plugin's main() method is invoked at install time to collect
	// metadata. The plugin will exit 0 and the Run([]string) method will not be
	// invoked.

	// About debug Locally:
	// The plugin interface hides panics from stdout, so in order to get panic info,
	// you can run this plugin outside of the plugin architecture by setting debuglocally = true.

	// example usage for local run: go run main.go download APP_NAME --overwrite 2> err.txt
	// note the lack of 'cf'

	debugLocally := false
	if debugLocally {
		var run DownloadPlugin
		run.Run(nil, os.Args[1:])
	} else {
		plugin.Start(new(DownloadPlugin))
	}

	// Plugin code should be written in the Run([]string) method,
	// ensuring the plugin environment is bootstrapped.
}
