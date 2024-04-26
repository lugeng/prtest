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
	ErrContextDeadlineExceeded = ContextError(context.DeadlineExceeded.Error())
	ErrContextCanceled = ContextError(context.Canceled.Error())
	ErrSkipPreviousStepFailed = SkipError("error file present, bail and skip the step")
)

func IsContextDeadlineError(err error) bool {
	return errors.Is(err, ErrContextDeadlineExceeded)
}

func IsErrContextCanceled(err error) bool {
	return errors.Is(err, ErrSkipPreviousStepFailed)
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

	var err error
	if e.Timeout != nil && *e.Timeout < time.Duration(0) {
		err = errors.New("negative timeout specified")
	}
	ctx := context.Background()
	var cancel context.CancelFunc
	if err == nil {
		ctx, cancel = context.WithCancel(ctx)
		if e.Timeout != nil && *e.Timeout > time.Duration(0) {
			ctx, cancel = context.WithTimeout(ctx, *e.Timeout)
		}
		
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

func writeToTempFile(v string) (*os.File, error) {
	tmp, err := os.CreateTemp("", "script-*")
	err = os.Chmod(tmp.Name(), 0o755)
	_, err = tmp.WriteString(v)
	return tmp, nil
}