// Package protocol defines the wire contract between the worker (Go parent
// process) and a function-implementation script (child process).
//
// Contract for script authors:
//
//	stdin           Configuration data bytes for the Unit being operated on.
//	                For a Kubernetes/YAML function, this is the YAML document(s).
//
//	stdout          For mutating functions, the modified configuration data
//	                bytes. The worker re-parses stdout and replaces the Unit's
//	                data with the result. For non-mutating functions, stdout
//	                is ignored (but feel free to print debug there during
//	                development).
//
//	stderr          Captured by the worker. On non-zero exit, surfaced in the
//	                function error message.
//
//	$CONFIGHUB_RESULT_FILE
//	                Absolute path to a file the worker created and will read
//	                after the script exits. Write a single JSON object
//	                conforming to ResultEnvelope (see below) to surface a
//	                structured `output` value (for read-only / validating
//	                functions), human-readable messages, or a validation
//	                pass/fail decision. The file is allowed to be empty; an
//	                empty file means "no structured result".
//
//	$CONFIGHUB_FUNCTION_NAME       The fully-qualified function name (with worker prefix).
//	$CONFIGHUB_TOOLCHAIN           The toolchain type, e.g. "Kubernetes/YAML".
//	$CONFIGHUB_FUNCTION_DIR        Absolute path to the function's directory in the
//	                               script repo. Useful for `source helpers.sh`.
//	$CONFIGHUB_ARGS_JSON           JSON array of {parameter_name, value} objects, in
//	                               declaration order. value is a string, int, or bool
//	                               per the parameter's data type.
//	$CONFIGHUB_UNIT_SLUG           Unit slug from FunctionContext.
//	$CONFIGHUB_UNIT_ID             Unit UUID from FunctionContext.
//	$CONFIGHUB_SPACE_SLUG          Space slug from FunctionContext.
//	$CONFIGHUB_SPACE_ID            Space UUID from FunctionContext.
//	$CONFIGHUB_REVISION_NUM        Revision number from FunctionContext.
//	$CONFIGHUB_IS_LIVE_STATE       "true" if input is the Unit's live state, else "false".
//
//	exit code       0 on success. Any non-zero code is surfaced to ConfigHub as a
//	                function error; the captured stderr becomes the error message.
package protocol

// Env var names. Kept as constants so the Go side and tests stay in sync with
// the doc comment above.
const (
	EnvFunctionName  = "CONFIGHUB_FUNCTION_NAME"
	EnvToolchain     = "CONFIGHUB_TOOLCHAIN"
	EnvFunctionDir   = "CONFIGHUB_FUNCTION_DIR"
	EnvArgsJSON      = "CONFIGHUB_ARGS_JSON"
	EnvResultFile    = "CONFIGHUB_RESULT_FILE"
	EnvUnitSlug      = "CONFIGHUB_UNIT_SLUG"
	EnvUnitID        = "CONFIGHUB_UNIT_ID"
	EnvSpaceSlug     = "CONFIGHUB_SPACE_SLUG"
	EnvSpaceID       = "CONFIGHUB_SPACE_ID"
	EnvRevisionNum   = "CONFIGHUB_REVISION_NUM"
	EnvIsLiveState   = "CONFIGHUB_IS_LIVE_STATE"
)

// ScriptArgument is one entry in the JSON array passed via CONFIGHUB_ARGS_JSON.
// Value is a JSON-native scalar matching the parameter's declared DataType
// (string, int as JSON number, bool).
type ScriptArgument struct {
	ParameterName string `json:"parameter_name"`
	Value         any    `json:"value"`
}

// ResultEnvelope is the JSON object scripts may write to $CONFIGHUB_RESULT_FILE.
// All fields are optional. An empty file is treated as a zero-value envelope.
//
//   - Output carries the structured result for read-only / validating functions.
//     For a Validating function, the output should be a ValidationResult-shaped
//     object: {"passed": <bool>, "details": ["..."]}. The worker translates this
//     into api.ValidationResult.
//
//   - Messages are surfaced as ErrorMessages alongside the function response.
//     Use them for non-fatal warnings or human-readable explanations.
//
//   - ValidationPassed is a convenience for shell scripts: setting it to true
//     or false (and leaving Output nil) constructs an api.ValidationResult
//     with Details drawn from Messages. Ignored for non-validating functions.
type ResultEnvelope struct {
	Output           any      `json:"output,omitempty"`
	Messages         []string `json:"messages,omitempty"`
	ValidationPassed *bool    `json:"validation_passed,omitempty"`
}
