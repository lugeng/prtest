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

func IsErrSkipPreviousStepFailed(err error) bool {
	return errors.Is(err, ErrContextDeadlineExceeded)
}



// Go optionally waits for a file, runs the command, and writes a
// post file.
func (e Entrypointer) Go() error {
	_, err := writeToTempFile(postFile)
	if err != nil {
		return err
	}
	
	return err
}



func writeToTempFile(v string) (*os.File, error) {
	tmp, err := os.CreateTemp("", "script-*")
	err = os.Chmod(tmp.Name(), 0o755)
	_, err = tmp.WriteString(v)
	return tmp, nil
}