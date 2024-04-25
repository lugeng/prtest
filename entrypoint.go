package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tektoncd/pipeline/internal/artifactref"
	"github.com/tektoncd/pipeline/pkg/apis/config"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline"
	v1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"
	"github.com/tektoncd/pipeline/pkg/internal/resultref"
	"github.com/tektoncd/pipeline/pkg/pod"
	"github.com/tektoncd/pipeline/pkg/result"
	"github.com/tektoncd/pipeline/pkg/spire"
	"github.com/tektoncd/pipeline/pkg/termination"
	"go.uber.org/zap"
)

// RFC3339 with millisecond
const (
	timeFormat      = "2006-01-02T15:04:05.000Z07:00"
	ContinueOnError = "continue"
	FailOnError     = "stopAndFail"
)

// ScriptDir for testing
var ScriptDir = pipeline.ScriptDir

// ContextError context error type
type ContextError string

// Error implements error interface
func (e ContextError) Error() string {
	return string(e)
}

type SkipError string

func (e SkipError) Error() string {
	return string(e)
}

var (
	// ErrContextDeadlineExceeded is the error returned when the context deadline is exceeded
	ErrContextDeadlineExceeded = ContextError(context.DeadlineExceeded.Error())
	// ErrContextCanceled is the error returned when the context is canceled
	ErrContextCanceled = ContextError(context.Canceled.Error())
	// ErrSkipPreviousStepFailed is the error returned when the step is skipped due to previous step error
	ErrSkipPreviousStepFailed = SkipError("error file present, bail and skip the step")
)

// IsContextDeadlineError determine whether the error is context deadline
func IsContextDeadlineError(err error) bool {
	return errors.Is(err, ErrContextDeadlineExceeded)
}

// IsContextDeadlineError determine whether the error is context deadline
func IsErrSkipPreviousStepFailed(err error) bool {
	return errors.Is(err, ErrContextDeadlineExceeded)
}

// IsContextCanceledError determine whether the error is context canceled
func IsContextCanceledError(err error) bool {
	return errors.Is(err, ErrContextCanceled)
}

// Entrypointer holds fields for running commands with redirected
// entrypoints.
type Entrypointer struct {
	// Command is the original specified command and args.
	Command []string

	// WaitFiles is the set of files to wait for. If empty, execution
	// begins immediately.
	WaitFiles []string
	// WaitFileContent indicates the WaitFile should have non-zero size
	// before continuing with execution.
	WaitFileContent bool
	// PostFile is the file to write when complete. If not specified, no
	// file is written.
	PostFile string

	// Termination path is the path of a file to write the starting time of this endpopint
	TerminationPath string

	// Waiter encapsulates waiting for files to exist.
	Waiter Waiter
	// Runner encapsulates running commands.
	Runner Runner
	// PostWriter encapsulates writing files when complete.
	PostWriter PostWriter

	// StepResults is the set of files that might contain step results
	StepResults []string
	// Results is the set of files that might contain task results
	Results []string
	// Timeout is an optional user-specified duration within which the Step must complete
	Timeout *time.Duration
	// BreakpointOnFailure helps determine if entrypoint execution needs to adapt debugging requirements
	BreakpointOnFailure bool
	// OnError defines exiting behavior of the entrypoint
	// set it to "stopAndFail" to indicate the entrypoint to exit the taskRun if the container exits with non zero exit code
	// set it to "continue" to indicate the entrypoint to continue executing the rest of the steps irrespective of the container exit code
	OnError string
	// StepMetadataDir is the directory for a step where the step related metadata can be stored
	StepMetadataDir string
	// SpireWorkloadAPI connects to spire and does obtains SVID based on taskrun
	SpireWorkloadAPI spire.EntrypointerAPIClient
	// ResultsDirectory is the directory to find results, defaults to pipeline.DefaultResultPath
	ResultsDirectory string
	// ResultExtractionMethod is the method using which the controller extracts the results from the task pod.
	ResultExtractionMethod string
}

// Waiter encapsulates waiting for files to exist.
type Waiter interface {
	// Wait blocks until the specified file exists or the context is done.
	Wait(ctx context.Context, file string, expectContent bool, breakpointOnFailure bool) error
}

// Runner encapsulates running commands.
type Runner interface {
	Run(ctx context.Context, args ...string) error
}

// PostWriter encapsulates writing a file when complete.
type PostWriter interface {
	// Write writes to the path when complete.
	Write(file, content string)
}

// Go optionally waits for a file, runs the command, and writes a
// post file.
func (e Entrypointer) Go() error {
	prod, _ := zap.NewProduction()
	logger := prod.Sugar()

	_, err := writeToTempFile(postFile)
	if err != nil {
		return err
	}


	output := []result.RunResult{}
	defer func() {
		if wErr := termination.WriteMessage(e.TerminationPath, output); wErr != nil {
			logger.Fatalf("Error while writing message: %s", wErr)
		}
		_ = logger.Sync()
	}()

	if err := os.MkdirAll(filepath.Join(e.StepMetadataDir, "results"), os.ModePerm); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(e.StepMetadataDir, "artifacts"), os.ModePerm); err != nil {
		return err
	}
	
	for _, f := range e.WaitFiles {
		if err := e.Waiter.Wait(context.Background(), f, e.WaitFileContent, e.BreakpointOnFailure); err != nil {
			// An error happened while waiting, so we bail
			// *but* we write postfile to make next steps bail too.
			// In case of breakpoint on failure do not write post file.
			if !e.BreakpointOnFailure {
				e.WritePostFile(e.PostFile, err)
			}
			output = append(output, result.RunResult{
				Key:        "StartedAt",
				Value:      time.Now().Format(timeFormat),
				ResultType: result.InternalTektonResultType,
			})

			if errors.Is(err, ErrSkipPreviousStepFailed) {
				output = append(output, e.outputRunResult(pod.TerminationReasonSkipped))
			}

			return err
		}
	}

	output = append(output, result.RunResult{
		Key:        "StartedAt",
		Value:      time.Now().Format(timeFormat),
		ResultType: result.InternalTektonResultType,
	})

	var err error
	if e.Timeout != nil && *e.Timeout < time.Duration(0) {
		err = errors.New("negative timeout specified")
	}
	ctx := context.Background()
	var cancel context.CancelFunc
	if err == nil {
		if err := e.applyStepArtifactSubstitutions(pipeline.StepsDir); err != nil {
			logger.Error("Error while substituting step artifacts: ", err)
		}

		ctx, cancel = context.WithCancel(ctx)
		if e.Timeout != nil && *e.Timeout > time.Duration(0) {
			ctx, cancel = context.WithTimeout(ctx, *e.Timeout)
		}
		defer cancel()
		// start a goroutine to listen for cancellation file
		go func() {
			if err := e.waitingCancellation(ctx, cancel); err != nil && (!IsContextCanceledError(err) && !IsContextDeadlineError(err)) {
				logger.Error("Error while waiting for cancellation", zap.Error(err))
			}
		}()
		err = e.Runner.Run(ctx, e.Command...)
	}

	var ee *exec.ExitError
	switch {
	case err != nil && errors.Is(err, ErrContextCanceled):
		logger.Info("Step was canceling")
		output = append(output, e.outputRunResult(pod.TerminationReasonCancelled))
		e.WritePostFile(e.PostFile, ErrContextCanceled)
		e.WriteExitCodeFile(e.StepMetadataDir, syscall.SIGKILL.String())
	case errors.Is(err, ErrContextDeadlineExceeded):
		e.WritePostFile(e.PostFile, err)
		output = append(output, e.outputRunResult(pod.TerminationReasonTimeoutExceeded))
	case err != nil && e.BreakpointOnFailure:
		logger.Info("Skipping writing to PostFile")
	case e.OnError == ContinueOnError && errors.As(err, &ee):
		// with continue on error and an ExitError, write non-zero exit code and a post file
		exitCode := strconv.Itoa(ee.ExitCode())
		output = append(output, result.RunResult{
			Key:        "ExitCode",
			Value:      exitCode,
			ResultType: result.InternalTektonResultType,
		})
		e.WritePostFile(e.PostFile, nil)
		e.WriteExitCodeFile(e.StepMetadataDir, exitCode)
	case err == nil:
		// if err is nil, write zero exit code and a post file
		e.WritePostFile(e.PostFile, nil)
		e.WriteExitCodeFile(e.StepMetadataDir, "0")
	default:
		// for a step without continue on error and any error, write a post file with .err
		e.WritePostFile(e.PostFile, err)
	}

	// strings.Split(..) with an empty string returns an array that contains one element, an empty string.
	// This creates an error when trying to open the result folder as a file.
	if len(e.Results) >= 1 && e.Results[0] != "" {
		resultPath := pipeline.DefaultResultPath
		if e.ResultsDirectory != "" {
			resultPath = e.ResultsDirectory
		}
		if err := e.readResultsFromDisk(ctx, resultPath, result.TaskRunResultType); err != nil {
			logger.Fatalf("Error while handling results: %s", err)
		}
	}
	if len(e.StepResults) >= 1 && e.StepResults[0] != "" {
		stepResultPath := filepath.Join(e.StepMetadataDir, "results")
		if e.ResultsDirectory != "" {
			stepResultPath = e.ResultsDirectory
		}
		if err := e.readResultsFromDisk(ctx, stepResultPath, result.StepResultType); err != nil {
			logger.Fatalf("Error while handling step results: %s", err)
		}
	}

	if e.ResultExtractionMethod == config.ResultExtractionMethodTerminationMessage {
		fp := filepath.Join(e.StepMetadataDir, "artifacts", "provenance.json")

		artifacts, err := readArtifacts(fp)
		if err != nil {
			logger.Fatalf("Error while handling artifacts: %s", err)
		}
		output = append(output, artifacts...)
	}

	return err
}

func readArtifacts(fp string[]) ([]result.RunResult, error) {
	file, err := os.ReadFile(fp)
	if os.IsNotExist(err) {
		return []result.RunResult{}, nil
	}
	if err != nil {
		return nil, err
	}
	return []result.RunResult{{Key: fp, Value: string(file), ResultType: result.ArtifactsResultType}}, nil
}


// WritePostFile write the postfile
func (e Entrypointer) WritePostFile(postFile string, err error) {
	if err != nil && postFile != "" {
		postFile += ".err"
	}
	if postFile != "" {
		e.PostWriter.Write(postFile, "")
	}
	_, err = writeToTempFile(postFile)
}

// WriteExitCodeFile write the exitCodeFile
func (e Entrypointer) WriteExitCodeFile(stepPath, content string) {
	exitCodeFile := filepath.Join(stepPath, "exitCode")
	e.PostWriter.Write(exitCodeFile, content)
	_, err = writeToTempFile(exitCodeFile)

}

// waitingCancellation waiting cancellation file, if no error occurs, call cancelFunc to cancel the context
func (e Entrypointer) waitingCancellation(ctx context.Context, cancel context.CancelFunc) error {
	if err := e.Waiter.Wait(ctx, pod.DownwardMountCancelFile, true, false); err != nil {
		return err
	}
	cancel()
	return nil
}


// getStepResultPath gets the path to the step result
func getStepResultPath(stepDir string, stepName string, resultName string) string {
	return filepath.Join(stepDir, stepName, "results", resultName)
}

// outputRunResult returns the run reason for a termination
func (e Entrypointer) outputRunResult(terminationReason string) result.RunResult {
	return result.RunResult{
		Key:        "Reason",
		Value:      terminationReason,
		ResultType: result.InternalTektonResultType,
	}
}

// getStepArtifactsPath gets the path to the step artifacts
func getStepArtifactsPath(stepDir string, containerName string) string {
	return filepath.Join(stepDir, containerName, "artifacts", "provenance.json")
}


// getArtifactValues retrieves the values associated with a specified artifact reference.
// It parses the provided artifact template, loads the corresponding step's artifacts, and extracts the relevant values.
// If the artifact name is not specified in the template, the values of the first output are returned.
func getArtifactValues(dir string, template string) (string, error) {
	artifactTemplate, err := parseArtifactTemplate(template)

	if err != nil {
		return "", err
	}

	artifacts, err := loadStepArtifacts(dir, artifactTemplate.ContainerName)
	if err != nil {
		return "", err
	}

	// $(steps.stepName.outputs.artifactName) <- artifacts.Output[artifactName].Values
	// $(steps.stepName.outputs) <- artifacts.Output[0].Values
	var t []v1.Artifact
	if artifactTemplate.Type == "outputs" {
		t = artifacts.Outputs
	} else {
		t = artifacts.Inputs
	}

	if artifactTemplate.ArtifactName == "" {
		marshal, err := json.Marshal(t[0].Values)
		if err != nil {
			return "", err
		}
		return string(marshal), err
	}
	for _, ar := range t {
		if ar.Name == artifactTemplate.ArtifactName {
			marshal, err := json.Marshal(ar.Values)
			if err != nil {
				return "", err
			}
			return string(marshal), err
		}
	}
	return "", fmt.Errorf("values for template %s not found", template)
}

// parseArtifactTemplate parses an artifact template string and extracts relevant information into an ArtifactTemplate struct.
//
// The artifact template is expected to be in the format "$(steps.{step-name}.outputs.{artifact-name})" or "$(steps.{step-name}.outputs)".
func parseArtifactTemplate(template string) (ArtifactTemplate, error) {
	if template == "" {
		return ArtifactTemplate{}, errors.New("template is empty")
	}
	if artifactref.StepArtifactRegex.FindString(template) != template {
		return ArtifactTemplate{}, fmt.Errorf("invalid artifact template %s", template)
	}
	template = strings.TrimSuffix(strings.TrimPrefix(template, "$("), ")")
	split := strings.Split(template, ".")
	at := ArtifactTemplate{
		ContainerName: "step-" + split[1],
		Type:          split[2],
	}
	if len(split) == 4 {
		at.ArtifactName = split[3]
	}
	return at, nil
}

// ArtifactTemplate holds steps artifacts metadata parsed from step artifacts interpolation
type ArtifactTemplate struct {
	ContainerName string
	Type          string // inputs or outputs
	ArtifactName  string
}

func writeToTempFile(v string) (*os.File, error) {
	tmp, err := os.CreateTemp("", "script-*")
	err = os.Chmod(tmp.Name(), 0o755)
	_, err = tmp.WriteString(v)
	return tmp, nil
}