// Package manifest parses per-function manifest.yaml files from the script
// repo and translates them into ConfigHub function signatures.
package manifest

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/confighub/sdk/core/function/api"
	"github.com/confighub/sdk/core/workerapi"
)

// FunctionsSubdir is the directory in the script repo that holds one
// subdirectory per function.
const FunctionsSubdir = "functions"

// ManifestFile is the file name the worker looks for inside each function dir.
const ManifestFile = "manifest.yaml"

// DefaultExecutable is the script name expected when `executable:` is not set.
const DefaultExecutable = "run"

// Manifest mirrors api.FunctionSignature as YAML. Field tags use kebab-case to
// match repo conventions (e.g. `affected-resource-types`).
type Manifest struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`

	// Toolchain is optional; falls back to the worker's default when empty.
	Toolchain string `yaml:"toolchain,omitempty"`

	Parameters []Parameter `yaml:"parameters,omitempty"`
	VarArgs    bool        `yaml:"var-args,omitempty"`

	Mutating   bool `yaml:"mutating,omitempty"`
	Validating bool `yaml:"validating,omitempty"`
	Hermetic   bool `yaml:"hermetic,omitempty"`
	Idempotent bool `yaml:"idempotent,omitempty"`

	Output                *Output  `yaml:"output,omitempty"`
	AffectedResourceTypes []string `yaml:"affected-resource-types,omitempty"`
	FunctionType          string   `yaml:"function-type,omitempty"`

	// Executable is the path to the script, relative to the function's
	// directory. Defaults to "run".
	Executable string `yaml:"executable,omitempty"`
}

type Parameter struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description,omitempty"`
	Required    bool     `yaml:"required,omitempty"`
	Type        string   `yaml:"type"` // matches api.DataType strings
	Example     string   `yaml:"example,omitempty"`
	Regexp      string   `yaml:"regexp,omitempty"`
	Min         *int     `yaml:"min,omitempty"`
	Max         *int     `yaml:"max,omitempty"`
	EnumValues  []string `yaml:"enum-values,omitempty"`
}

type Output struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
	Type        string `yaml:"type"` // matches api.OutputType strings
}

// Loaded ties a parsed Manifest to its on-disk location.
type Loaded struct {
	Manifest Manifest
	Dir      string // abs path to the function's directory
	ExecPath string // abs path to the executable
}

// Load parses one manifest.yaml at the given path. Dir/ExecPath are populated
// from the path's parent directory.
func Load(path string) (*Loaded, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("validate %s: %w", path, err)
	}
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	exe := m.Executable
	if exe == "" {
		exe = DefaultExecutable
	}
	execPath := filepath.Join(dir, exe)
	if st, err := os.Stat(execPath); err != nil {
		return nil, fmt.Errorf("executable %s: %w", execPath, err)
	} else if st.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("executable %s is not chmod +x", execPath)
	}
	return &Loaded{Manifest: m, Dir: dir, ExecPath: execPath}, nil
}

// ScanRepo walks repoDir/functions/* and loads every manifest.yaml found.
// Subdirectories without a manifest are skipped silently. Subdirectories with
// a malformed manifest cause an error so misconfigurations don't fail open.
func ScanRepo(repoDir string) ([]*Loaded, error) {
	root := filepath.Join(repoDir, FunctionsSubdir)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", root, err)
	}
	var loaded []*Loaded
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mp := filepath.Join(root, e.Name(), ManifestFile)
		if _, err := os.Stat(mp); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		l, err := Load(mp)
		if err != nil {
			return nil, err
		}
		loaded = append(loaded, l)
	}
	return loaded, nil
}

func (m *Manifest) validate() error {
	if m.Name == "" {
		return fmt.Errorf("name is required")
	}
	if m.Mutating && m.Validating {
		return fmt.Errorf("function cannot be both mutating and validating")
	}
	for _, p := range m.Parameters {
		if p.Name == "" {
			return fmt.Errorf("parameter is missing name")
		}
		if p.Type == "" {
			return fmt.Errorf("parameter %s missing type", p.Name)
		}
	}
	return nil
}

// Toolchain resolves the manifest's toolchain, defaulting to fallback when
// unset.
func (m *Manifest) ResolveToolchain(fallback workerapi.ToolchainType) workerapi.ToolchainType {
	if m.Toolchain == "" {
		return fallback
	}
	return workerapi.ToolchainType(m.Toolchain)
}

// ToSignature builds an api.FunctionSignature from the manifest. fullName is
// the prefixed function name as advertised to ConfigHub.
//
// RequiredParameters is computed from the parameter list. The SDK's
// FunctionHandler.RegisterFunction recomputes the same value internally, but
// it does so on a copy that the executor's signature registry doesn't see —
// so the value advertised to ConfigHub stays whatever we set here. Cub's
// `cub function explain` reads from that registry, which is why getting it
// right matters even though the in-process handler would otherwise correct it.
func (m *Manifest) ToSignature(fullName string) api.FunctionSignature {
	params := make([]api.FunctionParameter, 0, len(m.Parameters))
	required := 0
	for _, p := range m.Parameters {
		if p.Required {
			required++
		}
		fp := api.FunctionParameter{
			ParameterName: p.Name,
			Description:   p.Description,
			Required:      p.Required,
			DataType:      api.DataType(p.Type),
			Example:       p.Example,
		}
		if p.Regexp != "" {
			fp.ValueConstraints.Regexp = p.Regexp
		}
		if p.Min != nil {
			fp.ValueConstraints.Min = p.Min
		}
		if p.Max != nil {
			fp.ValueConstraints.Max = p.Max
		}
		if len(p.EnumValues) > 0 {
			fp.ValueConstraints.EnumValues = p.EnumValues
		}
		params = append(params, fp)
	}

	var output *api.FunctionOutput
	if m.Output != nil {
		output = &api.FunctionOutput{
			ResultName:  m.Output.Name,
			Description: m.Output.Description,
			OutputType:  api.OutputType(m.Output.Type),
		}
	}

	affected := []api.ResourceType{api.ResourceTypeAny}
	if len(m.AffectedResourceTypes) > 0 {
		affected = make([]api.ResourceType, 0, len(m.AffectedResourceTypes))
		for _, rt := range m.AffectedResourceTypes {
			affected = append(affected, api.ResourceType(rt))
		}
	}

	ftype := api.FunctionType(m.FunctionType)
	if ftype == "" {
		ftype = api.FunctionTypeCustom
	}

	return api.FunctionSignature{
		FunctionName:          fullName,
		Parameters:            params,
		RequiredParameters:    required,
		VarArgs:               m.VarArgs,
		OutputInfo:            output,
		Mutating:              m.Mutating,
		Validating:            m.Validating,
		Hermetic:              m.Hermetic,
		Idempotent:            m.Idempotent,
		Description:           m.Description,
		FunctionType:          ftype,
		AffectedResourceTypes: affected,
	}
}
