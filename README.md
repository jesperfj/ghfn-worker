# ghfn-worker

A ConfigHub function worker that pulls function implementations from a
GitHub repository and registers each as a worker-scoped function with
ConfigHub. Authoring a new function is `git push`; existing functions can
be reloaded without redeploying the worker.

- **Image**: `ghcr.io/jesperfj/ghfn-worker:<version>` (multi-arch: linux/amd64, linux/arm64)
- **Helm chart**: `oci://ghcr.io/jesperfj/charts/ghfn-worker:<version>`
- **Function repo layout**: `functions/<name>/{manifest.yaml, run}`
- **Toolchain**: `Kubernetes/YAML` (default; manifest can override)

## Contents

- [How it works](#how-it-works)
- [Quick start](#quick-start)
- [Function repo layout](#function-repo-layout)
  - [`manifest.yaml` schema](#manifestyaml-schema)
  - [`run` script wire protocol](#run-script-wire-protocol)
- [Configuration reference](#configuration-reference)
- [Operations](#operations)
  - [Refresh after pushing function changes](#refresh-after-pushing-function-changes)
  - [Logs and pod state](#logs-and-pod-state)
- [Local development on the worker itself](#local-development-on-the-worker-itself)
- [Releases](#releases)

## How it works

1. On startup, the worker either clones `REPO_URL` into `REPO_DIR` (git mode)
   or treats `LOCAL_REPO_PATH` as an already-checked-out repo root (local mode).
2. It scans `<repo>/functions/*/manifest.yaml`, parses each manifest into a
   `FunctionSignature`, and advertises the resulting set of functions to
   ConfigHub when it connects.
3. When ConfigHub invokes a function, the worker `exec`'s the function's
   `run` script, piping the unit's config data on stdin and the parameter
   values via the `CONFIGHUB_ARGS_JSON` env var. Stdout is the function's
   output (mutated YAML for mutating functions, freeform for the rest).
4. A reserved builtin `refresh-function-repo` re-pulls the repo. Adding or
   removing a function (signature change) triggers a worker pod restart so
   the new signature set is re-advertised; body-only edits to existing
   scripts are picked up immediately on the next invocation.

The worker is a single Go binary (~10 MB, statically linked) plus a small
Alpine runtime that bundles `bash`, `python3`, `git`, `jq`, and `yq` — the
tools function authors are most likely to reach for.

## Quick start

You'll need:

- a Kubernetes cluster with a ConfigHub bridge worker target already
  provisioned (e.g., from `cub lk up`)
- `cub`, `kubectl`, and `helm` on your path
- a GitHub repository populated with `functions/<name>/{manifest.yaml, run}`
- (optional) a GitHub PAT if the repo is private

```sh
SPACE=ghfn-cluster              # ConfigHub space your bridge target lives in
TARGET=target                   # bridge target name
RELEASE=ghfn                    # Helm release name (also becomes the cub worker slug)
NS=default                      # Kubernetes namespace
REPO_URL=https://github.com/<owner>/<functions-repo>
VERSION=0.0.5

# 1. Render the chart into a ConfigHub unit.
cub helm install \
  --space "$SPACE" \
  --namespace "$NS" \
  --target "$TARGET" \
  "$RELEASE" \
  oci://ghcr.io/jesperfj/charts/ghfn-worker \
  --version "$VERSION"

# 2. Register the worker so ConfigHub can authenticate it.
cub worker create --space "$SPACE" "$RELEASE"

# 3. Materialize the worker Secret in the cluster. The chart references
#    "<release>-ghfn-worker-secret"; rename from cub's default to match.
WORKER_SECRET=$(cub worker install --export-secret-only --space "$SPACE" "$RELEASE" \
  | yq '.stringData.CONFIGHUB_WORKER_SECRET')
kubectl create secret generic "${RELEASE}-ghfn-worker-secret" -n "$NS" \
  --from-literal=CONFIGHUB_WORKER_SECRET="$WORKER_SECRET" \
  --from-literal=token="$GITHUB_PAT"     # omit if the functions repo is public

# 4. Fill in the placeholder env values via cub function do.
WID=$(cub worker get --space "$SPACE" "$RELEASE" -o json | jq -r .BridgeWorker.BridgeWorkerID)
cub function do --space "$SPACE" --where "Slug = '$RELEASE'" -- \
  set-string-path apps/v1/Deployment \
  'spec.template.spec.containers.?name=worker.env.?name=CONFIGHUB_WORKER_ID.value' "$WID"
cub function do --space "$SPACE" --where "Slug = '$RELEASE'" -- \
  set-string-path apps/v1/Deployment \
  'spec.template.spec.containers.?name=worker.env.?name=REPO_URL.value' "$REPO_URL"

# 5. Apply the unit through the bridge target. The pod comes up, clones the
#    functions repo, and connects to ConfigHub.
cub unit apply --space "$SPACE" "$RELEASE"
```

Verify:

```sh
cub worker get --space "$SPACE" "$RELEASE" -o json | jq '.BridgeWorker.Condition'
# "Ready"
cub function list --space "$SPACE" --worker "$RELEASE"
# all functions/<name> entries plus the builtin refresh-function-repo
```

## Function repo layout

```
functions/
  hello-bash/
    manifest.yaml
    run                # chmod +x; first line is its shebang
  count-resources/
    manifest.yaml
    run
  ...
```

Anything that's `chmod +x` works as `run`: bash, python, a compiled binary,
etc. The runtime image bundles `bash`, `python3`, `jq`, and `yq` so most
config-massaging functions need nothing extra.

### `manifest.yaml` schema

```yaml
name: hello-bash                     # required; the function's name as advertised to ConfigHub
description: short one-liner         # required
toolchain: Kubernetes/YAML           # optional; defaults to DEFAULT_TOOLCHAIN env

mutating: true                       # function rewrites the input
validating: false                    # function emits validation findings
hermetic: true                       # depends only on its inputs
idempotent: true                     # running twice == running once

parameters:
  - name: greeting
    description: text to use as the annotation value
    required: true
    type: string                     # also: int, bool, etc. — matches ConfigHub DataType
    example: "Hello!"
    # optional constraints
    regexp: '^[A-Za-z ]+$'
    min: 1
    max: 64
    enum-values: ["a", "b"]

# Optional. Mutating functions always emit YAML on stdout regardless of
# this declaration; non-mutating functions use it to describe the type of
# their stdout (YAML, JSON, plain text, etc.).
output:
  name: modified-config
  description: input with greeting annotation
  type: YAML

affected-resource-types:             # optional hint for ConfigHub UI
  - apps/v1/Deployment
  - v1/Service

executable: run                      # optional; defaults to the file `run` in the same dir
```

### `run` script wire protocol

The full contract lives in `internal/protocol/protocol.go`. Scripts
communicate with the worker via stdin, stdout, a result file, and a set of
`CONFIGHUB_*` env vars.

| Stream / env var          | Direction | Purpose                                                                                                                                  |
| ------------------------- | --------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| `stdin`                   | in        | The unit's config data (typically a multi-doc YAML stream).                                                                              |
| `stdout`                  | out       | **Mutating functions**: the rewritten config — re-parsed by the worker and written back to the unit. **Non-mutating**: ignored (use it for debug). |
| `stderr`                  | out       | Captured by the worker. On non-zero exit, becomes the function's error message surfaced to ConfigHub.                                    |
| `$CONFIGHUB_RESULT_FILE`  | out       | Path to a worker-created file the script writes a [`ResultEnvelope`](#resultenvelope-shape) JSON into. Empty file = no structured result. |
| `$CONFIGHUB_ARGS_JSON`    | in        | JSON array `[{"parameter_name":"x","value":<scalar>}, ...]` — arguments by name in declaration order. Value type matches the parameter's declared `type`. |
| `$CONFIGHUB_FUNCTION_NAME`| in        | The fully-qualified function name as ConfigHub sees it.                                                                                  |
| `$CONFIGHUB_TOOLCHAIN`    | in        | Toolchain type, e.g. `Kubernetes/YAML`.                                                                                                  |
| `$CONFIGHUB_FUNCTION_DIR` | in        | Absolute path to the function's directory in the cloned repo. Useful for `source helpers.sh` or sibling-file lookups.                   |
| `$CONFIGHUB_UNIT_SLUG`    | in        | Unit slug from the `FunctionContext`.                                                                                                    |
| `$CONFIGHUB_UNIT_ID`      | in        | Unit UUID.                                                                                                                                |
| `$CONFIGHUB_SPACE_SLUG`   | in        | Space slug.                                                                                                                              |
| `$CONFIGHUB_SPACE_ID`     | in        | Space UUID.                                                                                                                              |
| `$CONFIGHUB_REVISION_NUM` | in        | Revision number.                                                                                                                          |
| `$CONFIGHUB_IS_LIVE_STATE`| in        | `"true"` if the input is the unit's live state (e.g., for validation against deployed state) rather than its desired config.            |
| exit code                 | out       | `0` on success. Non-zero is a function failure; the captured stderr becomes the error message.                                          |

#### `ResultEnvelope` shape

The script writes JSON to `$CONFIGHUB_RESULT_FILE`. All fields are optional;
an empty file means "no structured result".

```jsonc
{
  "output": <any>,                 // structured result for read-only / validating functions
  "messages": ["...", "..."],      // human-readable notes; surfaced as ErrorMessages
  "validation_passed": true        // shorthand for validating functions; ignored otherwise
}
```

For a validating function, either set `output` to a `{"passed": <bool>, "details": [...]}`
object, or set `validation_passed` and let the worker construct the
ValidationResult from `messages`.

#### Examples

Mutating bash function (`functions/hello-bash/run`) — writes back to
stdout, no result file:

```sh
#!/usr/bin/env bash
set -euo pipefail
export greeting=$(echo "$CONFIGHUB_ARGS_JSON" \
  | jq -r '.[] | select(.parameter_name=="greeting") | .value')
yq eval-all '.metadata.annotations["example.com/greeting"] = strenv(greeting)' -
```

Non-mutating python function returning a structured `output` plus a
human-readable note:

```python
#!/usr/bin/env python3
import json, os, sys
data = sys.stdin.read()
docs = [d for d in data.split("\n---") if d.strip()]
envelope = {
    "output": {"count": len(docs)},
    "messages": [f"counted {len(docs)} resource(s)"],
}
with open(os.environ["CONFIGHUB_RESULT_FILE"], "w") as fh:
    json.dump(envelope, fh)
```

Non-mutating bash equivalent using `jq`:

```sh
#!/usr/bin/env bash
set -euo pipefail
kinds=$(yq eval-all '.kind' - | sort -u | jq -R . | jq -s .)
jq -n --argjson k "$kinds" '{output: {kinds: $k}}' > "$CONFIGHUB_RESULT_FILE"
```

## Configuration reference

Every value is set via env var on the worker container. The chart defaults
the operational ones; you fill in the deployment-specific ones post-render.

| Env var                 | Required | Default                              | Description                                                                                                       |
| ----------------------- | :------: | ------------------------------------ | ----------------------------------------------------------------------------------------------------------------- |
| `CONFIGHUB_WORKER_ID`   | yes      | (placeholder)                        | UUID returned by `cub worker create`.                                                                              |
| `CONFIGHUB_WORKER_SECRET` | yes    | (from Secret)                        | Bearer secret for the worker. Mounted from a Kubernetes Secret, never templated into the manifest.                |
| `CONFIGHUB_URL`         | yes      | `https://hub.confighub.com`          | ConfigHub server.                                                                                                  |
| `REPO_URL`              | yes\*    | (placeholder)                        | HTTPS URL of the functions repo. \*Required unless `LOCAL_REPO_PATH` is set.                                       |
| `REPO_BRANCH`           | no       | `main`                               | Branch to track.                                                                                                   |
| `REPO_DIR`              | no       | `/var/lib/confighub/function-repo`   | Where the worker clones the repo. The chart pins this to `/repo` (a UID-1000-owned directory baked into the image). |
| `GITHUB_TOKEN`          | no       | unset                                | PAT for private repos. Falls back to `REPO_TOKEN`. Mounted from the same Secret, key `token`, optional.            |
| `LOCAL_REPO_PATH`       | no       | unset                                | Selects local-dev mode: the worker reads `functions/` directly from this path and skips git entirely.              |
| `DEFAULT_TOOLCHAIN`     | no       | `Kubernetes/YAML`                    | Used when a `manifest.yaml` omits `toolchain`.                                                                     |

If both `LOCAL_REPO_PATH` and `REPO_URL` are set, `LOCAL_REPO_PATH` wins.

The chart additionally mounts an `emptyDir` volume at `/local-functions`,
unused in production. Switch it to a `hostPath` and set
`LOCAL_REPO_PATH=/local-functions` to bypass git for fast on-laptop
iteration without changing the image or chart.

## Operations

### Refresh after pushing function changes

The worker only reads the function repo at startup — there's no in-place
re-pull. The `refresh-function-repo` builtin (and `SIGHUP` to PID 1) work
by exiting cleanly so the kubelet restarts the container; on the next
start, the worker re-pulls (git mode) or re-scans (local mode) and
re-advertises the function set to ConfigHub.

```sh
cub function do --space "$SPACE" --worker "$RELEASE" \
  --where "Slug = '$RELEASE'" -- refresh-function-repo
# or, equivalently, send SIGHUP:
kubectl exec -n "$NS" deploy/"$RELEASE"-ghfn-worker -- kill -HUP 1
```

The exit is delayed a couple of seconds so the SSE response from the
refresh call has time to flush back to ConfigHub. Any in-flight function
invocations that are still running after the grace period are killed;
keep functions short.

What you need to do depends on the mode:

| Change                                         | Git mode                       | Local mode (`LOCAL_REPO_PATH`)              |
| ---------------------------------------------- | ------------------------------ | ------------------------------------------- |
| Edited a `run` script body (no manifest change)| `refresh-function-repo`        | Picked up automatically on next invocation. |
| Edited a `manifest.yaml` (parameters, etc.)    | `refresh-function-repo`        | `refresh-function-repo`                     |
| Added or removed a function directory          | `refresh-function-repo`        | `refresh-function-repo`                     |

In other words: in git mode every change requires a refresh, since the
worker has no way to see your push otherwise. In local mode, body-only
edits to `run` scripts take effect immediately because the worker
exec's the file fresh on each invocation; only manifest/signature changes
need a refresh, because the advertised function set is computed once at
startup.

### Logs and pod state

```sh
kubectl get pod -n "$NS" -l app.kubernetes.io/instance="$RELEASE"
kubectl logs -n "$NS" -l app.kubernetes.io/instance="$RELEASE" -f
cub worker get --space "$SPACE" "$RELEASE" -o json | jq '.BridgeWorker.Condition'
```

The first few lines of the worker log on a healthy start show the clone,
the function inventory, and the connection to ConfigHub:

```
Cloning into '/repo'...
repo synced: branch=main sha=<sha> cloned=true
registered 3 functions: count-resources, hello-bash, list-kinds
registered builtin: refresh-function-repo (toolchain=Kubernetes/YAML)
[INFO] Successfully connected to event stream in <ms>ms, status: 200 200 OK
```

## Local development on the worker itself

If you're hacking on this repo (not just authoring functions in another
repo), the `Makefile` drives a kind-cluster + cub-lk loop:

```sh
make image                    # docker build into ghcr.io/jesperfj/ghfn-worker:dev
make kind-load                # load it into the kind cluster `ghfn`
make worker-create            # cub worker create
make secret                   # cub worker install --export-secret-only | rename | kubectl apply
make render                   # helm template + yq-patch the placeholders into .rendered/manifest.yaml
make deploy                   # = kind-load + secret + render + kubectl apply + rollout status
make logs                     # tail worker logs
make refresh                  # SIGHUP the worker
make undeploy && make worker-delete   # tear down cub side
make cluster-down             # tear down kind + lk
```

`make render` mirrors the production install pattern: it runs `helm
template` and then yq-patches `CONFIGHUB_WORKER_ID`, `REPO_URL`, and
`REPO_BRANCH` into the rendered manifest before applying — exactly the
shape that `cub function do set-string-path` produces in the cub flow.

The Helm chart lives at `chart/ghfn-worker/`. Its `values.yaml` is
intentionally near-empty: the chart renders deterministic manifests with
`confighubplaceholder` strings where deployment-specific values go, and
expects callers to fill them in post-render via cub functions or
`kubectl set env` rather than via Helm `--set`.

## Releases

Tag a `v*`-prefixed semver to release. The CI workflow at
`.github/workflows/release.yml` runs on tag push and pushes both:

- the multi-arch container image to `ghcr.io/jesperfj/ghfn-worker:<version>` (and `:latest`)
- the Helm chart to `oci://ghcr.io/jesperfj/charts/ghfn-worker:<version>`

The image and chart always ship lockstep at the same semver — the chart's
`version` and `appVersion` are overridden at `helm package` time from the
git tag, so the committed `Chart.yaml` stays at its `0.0.0-dev` placeholder.

The container build cross-compiles natively (build stage pinned to
`$BUILDPLATFORM`, Go target arch passed via `GOOS`/`GOARCH`) instead of
relying on QEMU emulation, which keeps a release under three minutes.
