/**
 * Copyright 2022 Gravitational, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package types

import (
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/gravitational/teleport/api/defaults"
	"github.com/gravitational/trace"
)

// IsStrictKebabCase checks if the given string meets a fairly strict definition of
// kebab case (no dots, dashes, caps, etc). Useful for strings that might need to be
// included in filenames.
var IsStrictKebabCase = regexp.MustCompile(`^[a-z0-9\-]+$`).MatchString

func (e *ExecScript) Check() error {
	if !IsStrictKebabCase(e.Type) {
		// type name is used in a filename, so we need to be strict
		// about its allowed characters.
		return trace.BadParameter("invalid type %q in exec-script message", e.Type)
	}

	if e.Script == "" {
		return trace.BadParameter("missing required field 'script' in exec-script message")
	}

	for name := range e.Env {
		if !isValidEnvVarName(name) {
			return trace.BadParameter("invalid env var name %q in exec-script message", name)
		}
	}

	for _, name := range e.EnvPassthrough {
		if !isValidEnvVarName(name) {
			return trace.BadParameter("invalid env passthrough var name %q in exec-script message", name)
		}
	}

	return nil
}

// isValidEnvVarName checks if the given name is valid for use in scipt installers.
func isValidEnvVarName(name string) bool {
	if name == "" {
		return false
	}

	for _, c := range name {
		if c == '=' || unicode.IsSpace(c) {
			return false
		}
	}

	return true
}

type LocalScriptInstaller interface {
	Resource
}

// NewLocalScriptInstaller constructs a new LocalScriptInstaller from the provided spec.
func NewLocalScriptInstaller(name string, spec LocalScriptInstallerSpecV1) (LocalScriptInstaller, error) {
	installer := &LocalScriptInstallerV1{
		ResourceHeader: ResourceHeader{
			Metadata: Metadata{
				Name: name,
			},
		},
		Spec: spec,
	}

	if err := installer.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}

	return installer, nil
}

func (i *LocalScriptInstallerV1) CheckAndSetDefaults() error {
	i.setStaticFields()

	if err := i.ResourceHeader.CheckAndSetDefaults(); err != nil {
		return trace.Wrap(err)
	}

	if i.Version != V1 {
		return trace.BadParameter("unsupported local script installer version: %s", i.Version)
	}

	if i.Kind != KindLocalScriptInstaller {
		return trace.BadParameter("unexpected resource kind: %q (expected %s)", i.Kind, KindLocalScriptInstaller)
	}

	if i.Metadata.Namespace != "" && i.Metadata.Namespace != defaults.Namespace {
		return trace.BadParameter("invalid namespace %q (namespaces are deprecated)", i.Metadata.Namespace)
	}

	if i.Spec.InstallScript == "" {
		return trace.BadParameter("missing required field 'install.sh' in local script installer")
	}

	if i.Spec.RestartScript == "" {
		return trace.BadParameter("missing required field 'restart.sh' in local script installer")
	}

	for name := range i.Spec.Env {
		if !isValidEnvVarName(name) {
			return trace.BadParameter("invalid env var name %q in local script installer", name)
		}
	}

	for _, name := range i.Spec.EnvPassthrough {
		if !isValidEnvVarName(name) {
			return trace.BadParameter("invalid env passthrough var name %q in local script installer", name)
		}
	}

	if i.Spec.Shell != "" {
		// verify shell directive w/ optional shebang-style arg
		parts := strings.SplitN(strings.TrimSpace(i.Spec.Shell), " ", 2)
		if !filepath.IsAbs(parts[0]) {
			return trace.BadParameter("non-absolute shell path %q in local script installer", parts[0])
		}

		for _, arg := range parts[1:] {
			// some shebang impls bundle all space separated args after the executable
			// path into a single argument, and some allow for multiple args. the former
			// is more common, but the latter is generally regarded as superior. we sidestep
			// the issue for now by simply disallowing additional spaces. this will allow
			// us to adopt either behavior in the future w/o breaking user-facing comatibility
			// (though care will need to be taken to ensure auth<->node compat).
			for _, c := range arg {
				if unicode.IsSpace(c) {
					return trace.BadParameter("invalid argument %q for shell of local script installer", arg)
				}
			}
		}
	}

	return nil
}

func (i *LocalScriptInstallerV1) setStaticFields() {
	if i.Version == "" {
		i.Version = V1
	}

	if i.Kind == "" {
		i.Kind = KindLocalScriptInstaller
	}
}
