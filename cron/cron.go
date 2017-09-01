package cron

import (
	"bufio"
	"fmt"
	"github.com/aptible/supercronic/crontab"
	"github.com/sirupsen/logrus"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	READ_BUFFER_SIZE = 64 * 1024
)

func startReaderDrain(wg *sync.WaitGroup, readerLogger *logrus.Entry, reader io.ReadCloser) {
	wg.Add(1)

	go func() {
		defer func() {
			wg.Done()
			if err := reader.Close(); err != nil {
				readerLogger.Errorf("failed to close pipe: %v", err)
			}
		}()

		bufReader := bufio.NewReaderSize(reader, READ_BUFFER_SIZE)

		for {
			line, isPrefix, err := bufReader.ReadLine()

			if err != nil {
				if strings.Contains(err.Error(), os.ErrClosed.Error()) {
					// The underlying reader might get
					// closed by e.g. Wait(), or even the
					// process we're starting, so we don't
					// log this.
				} else if err == io.EOF {
					// EOF, we don't need to log this
				} else {
					// Unexpected error: log it
					readerLogger.Errorf("failed to read pipe: %v", err)
				}

				break
			}

			readerLogger.Info(string(line))

			if isPrefix {
				readerLogger.Warn("last line exceeded buffer size, continuing...")
			}
		}
	}()
}

func runJob(context *crontab.Context, command string, jobLogger *logrus.Entry) error {
	jobLogger.Info("starting")

	cmd := exec.Command(context.Shell, "-c", command)

	// Run in a separate process group so that in interactive usage, CTRL+C
	// stops supercronic, not the children threads.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	env := os.Environ()
	for k, v := range context.Environ {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	cmd.Env = env

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	var wg sync.WaitGroup

	stdoutLogger := jobLogger.WithFields(logrus.Fields{"channel": "stdout"})
	startReaderDrain(&wg, stdoutLogger, stdout)

	stderrLogger := jobLogger.WithFields(logrus.Fields{"channel": "stderr"})
	startReaderDrain(&wg, stderrLogger, stderr)

	wg.Wait()

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("error running command: %v", err)
	}

	return nil
}

func StartJob(wg *sync.WaitGroup, context *crontab.Context, job *crontab.Job, exitChan chan interface{}, cronLogger *logrus.Entry) {
	wg.Add(1)

	go func() {
		defer wg.Done()

		var cronIteration uint64 = 0
		nextRun := time.Now()

		// NOTE: this (intentionally) does not run multiple instances of the
		// job concurrently
		for {
			nextRun = job.Expression.Next(nextRun)
			cronLogger.Debugf("job will run next at %v", nextRun)

			delay := nextRun.Sub(time.Now())
			if delay < 0 {
				cronLogger.Warningf("job took too long to run: it should have started %v ago", -delay)
				nextRun = time.Now()
				continue
			}

			select {
			case <-exitChan:
				cronLogger.Debug("shutting down")
				return
			case <-time.After(delay):
				// Proceed normally
			}

			jobLogger := cronLogger.WithFields(logrus.Fields{
				"iteration": cronIteration,
			})

			err := runJob(context, job.Command, jobLogger)

			if err == nil {
				jobLogger.Info("job succeeded")
			} else {
				jobLogger.Error(err)
			}

			cronIteration++
		}
	}()
}
