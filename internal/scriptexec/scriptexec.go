// Package scriptexec runs a function-implementation script as a child
// process, marshaling inputs and outputs across the protocol contract defined
// in package protocol.
package scriptexec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/confighub/sdk/core/function/api"
	"github.com/confighub/sdk/core/function/handler"
	"github.com/confighub/sdk/core/third_party/gaby"
	"github.com/confighub/sdk/core/workerapi"

	"github.com/jesperfj/ghfn-worker/internal/manifest"
	"github.com/jesperfj/ghfn-worker/internal/protocol"
)

// Entry resolves a function name to its on-disk implementation.
type Entry struct {
	FullName  string
	Toolchain workerapi.ToolchainType
	Manifest  manifest.Manifest
	Dir       string
	ExecPath  string
}

// Lookup returns the entry currently registered for fullName, or nil if it
// has been removed (e.g., by a refresh that purged a deleted function).
type Lookup func(fullName string) *Entry

// Make builds a handler.FunctionImplementation closure that resolves its
// Entry through lookup on every invocation. That indirection means a refresh
// can swap in a new script body for the same function name without restarting
// the worker.
func Make(fullName string, lookup Lookup) handler.FunctionImplementation {
	return func(fArgs handler.FunctionImplementationArguments) (gaby.Container, any, error) {
		entry := lookup(fullName)
		if entry == nil {
			return fArgs.ParsedData, nil, fmt.Errorf("function %s is no longer registered", fullName)
		}
		return run(context.Background(), entry, fArgs)
	}
}

func run(ctx context.Context, entry *Entry, fArgs handler.FunctionImplementationArguments) (gaby.Container, any, error) {
	// Serialize the parsed config back to bytes for stdin. The handler
	// already converted from native to YAML before calling us, so
	// ParsedData.String() yields the same wire format the script was
	// designed against.
	stdinBytes := []byte(fArgs.ParsedData.String())

	// Result file: created by us, consumed by us. Script is told the path.
	resultFile, err := os.CreateTemp("", "confighub-result-*.json")
	if err != nil {
		return fArgs.ParsedData, nil, fmt.Errorf("create result file: %w", err)
	}
	resultPath := resultFile.Name()
	_ = resultFile.Close()
	defer os.Remove(resultPath)

	argsJSON, err := buildArgsJSON(fArgs.Arguments)
	if err != nil {
		return fArgs.ParsedData, nil, fmt.Errorf("encode args: %w", err)
	}

	cmd := exec.CommandContext(ctx, entry.ExecPath)
	cmd.Dir = entry.Dir
	cmd.Stdin = bytes.NewReader(stdinBytes)

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	cmd.Env = scriptEnv(entry, fArgs, resultPath, argsJSON)

	runErr := cmd.Run()

	// Always try to read the result file; the script may have written
	// useful detail before failing.
	envelope, envErr := readEnvelope(resultPath)
	if envErr != nil {
		// Don't drop a real exec error in favor of an envelope parse error.
		if runErr == nil {
			return fArgs.ParsedData, nil, envErr
		}
		// Surface both.
		return fArgs.ParsedData, nil, fmt.Errorf("%w; also: %v", runErr, envErr)
	}

	if runErr != nil {
		stderr := strings.TrimSpace(stderrBuf.String())
		if stderr == "" {
			return fArgs.ParsedData, nil, runErr
		}
		return fArgs.ParsedData, nil, fmt.Errorf("%w: %s", runErr, stderr)
	}

	// Build the function output value. For Validating functions, prefer an
	// explicit api.ValidationResult shape; fall back to ValidationPassed.
	var fnOutput any
	if entry.Manifest.Validating {
		fnOutput, err = validationOutput(envelope)
		if err != nil {
			return fArgs.ParsedData, nil, err
		}
	} else if envelope.Output != nil {
		fnOutput = envelope.Output
	}

	// Mutating: parse stdout as the new config bytes.
	parsed := fArgs.ParsedData
	if entry.Manifest.Mutating {
		out := stdoutBuf.Bytes()
		if len(bytes.TrimSpace(out)) == 0 {
			// No output written — treat as no-op.
		} else {
			newParsed, err := gaby.ParseAll(out)
			if err != nil {
				return fArgs.ParsedData, nil, fmt.Errorf("parse script stdout as config: %w", err)
			}
			parsed = newParsed
		}
	}

	// Surface human-readable messages by joining them onto a single error
	// when the script reported a non-pass validation. For non-validating
	// functions, messages are advisory only — we ignore them rather than
	// fail the function. (The handler's error path is the only way to
	// propagate per-invocation messages today.)
	if len(envelope.Messages) > 0 && !entry.Manifest.Validating {
		// Print to stderr so operators see them in worker logs without
		// failing the call.
		fmt.Fprintf(os.Stderr, "[%s] %s\n", entry.FullName, strings.Join(envelope.Messages, "; "))
	}

	return parsed, fnOutput, nil
}

func validationOutput(env *protocol.ResultEnvelope) (any, error) {
	// If the script wrote a structured Output, trust it: re-marshal
	// through JSON to coerce into api.ValidationResult.
	if env.Output != nil {
		raw, err := json.Marshal(env.Output)
		if err != nil {
			return nil, fmt.Errorf("re-encode validation output: %w", err)
		}
		var vr api.ValidationResult
		if err := json.Unmarshal(raw, &vr); err != nil {
			return nil, fmt.Errorf("validation output is not ValidationResult-shaped: %w", err)
		}
		return vr, nil
	}
	// Otherwise use the convenience field.
	if env.ValidationPassed == nil {
		return nil, fmt.Errorf("validating function did not write validation_passed or output")
	}
	return api.ValidationResult{
		Passed:  *env.ValidationPassed,
		Details: env.Messages,
	}, nil
}

func readEnvelope(path string) (*protocol.ResultEnvelope, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &protocol.ResultEnvelope{}, nil
		}
		return nil, fmt.Errorf("read result file: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return &protocol.ResultEnvelope{}, nil
	}
	var env protocol.ResultEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("parse result file as JSON: %w", err)
	}
	return &env, nil
}

func buildArgsJSON(args []api.FunctionArgument) (string, error) {
	out := make([]protocol.ScriptArgument, 0, len(args))
	for _, a := range args {
		out = append(out, protocol.ScriptArgument{
			ParameterName: a.ParameterName,
			Value:         a.Value,
		})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func scriptEnv(entry *Entry, fArgs handler.FunctionImplementationArguments, resultPath, argsJSON string) []string {
	fc := fArgs.FunctionContext
	env := os.Environ()
	env = append(env,
		protocol.EnvFunctionName+"="+entry.FullName,
		protocol.EnvToolchain+"="+string(entry.Toolchain),
		protocol.EnvFunctionDir+"="+entry.Dir,
		protocol.EnvArgsJSON+"="+argsJSON,
		protocol.EnvResultFile+"="+resultPath,
	)
	if fc != nil {
		env = append(env,
			protocol.EnvUnitSlug+"="+fc.UnitSlug,
			protocol.EnvUnitID+"="+fc.UnitID.String(),
			protocol.EnvSpaceSlug+"="+fc.SpaceSlug,
			protocol.EnvSpaceID+"="+fc.SpaceID.String(),
			protocol.EnvRevisionNum+"="+strconv.FormatInt(fc.RevisionNum, 10),
			protocol.EnvIsLiveState+"="+strconv.FormatBool(fc.IsLiveState),
		)
	}
	return env
}
