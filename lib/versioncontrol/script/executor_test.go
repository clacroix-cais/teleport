/*
Copyright 2022 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package script

import (
	"math"
	"testing"

	"github.com/gravitational/teleport/api/types"
	"github.com/stretchr/testify/require"
)

func TestExecutor(t *testing.T) {
	tts := []struct {
		params  types.ExecScript
		success bool
		output  string
		code    int32
	}{
		{
			params: types.ExecScript{
				Type: "basic-shell",
				ID:   1,
				Env: map[string]string{
					"name": "alice",
				},
				Script: `echo "Hello ${name}!"`,
			},
			success: true,
			output:  "Hello alice!\n",
		},
		{
			params: types.ExecScript{
				Type:   "python-shebang",
				ID:     1,
				Shell:  `/usr/bin/env python3`,
				Script: `print("Hello from python!")`,
			},
			success: true,
			output:  "Hello from python!\n",
		},
		{
			params: types.ExecScript{
				Type:   "nonexistent-shell",
				ID:     1,
				Shell:  `/this/does/not/exist`,
				Script: `nothing(here)`,
			},
			success: false,
		},
	}

	executor := Executor{
		cfg: ExecutorConfig{
			Shell: "/bin/sh",
			Dir:   t.TempDir(),
		},
	}

	for _, tt := range tts {

		result := executor.Exec(tt.params)

		require.Equal(t, tt.success, result.Success, "result=%+v", result)

		if tt.success {
			out, err := executor.LoadOutput(tt.params.Type, tt.params.ID)
			require.NoError(t, err, "result=%+v", result)

			require.Equal(t, tt.output, out, "result=%+v", result)
		}

		require.Equal(t, tt.code, result.Code, "result=%+v", result)
	}
}

func TestRefs(t *testing.T) {
	tts := []struct {
		r ref
		s string
	}{
		{
			r: ref{
				Type: "basic-ref",
				ID:   123,
			},
			s: "basic-ref-123",
		},
		{
			r: ref{
				Type: "big-num",
				ID:   math.MaxUint64,
			},
			s: "big-num-18446744073709551615",
		},
		{
			r: ref{
				Type: "small-num",
				ID:   0,
			},
			s: "small-num-0",
		},
	}

	for _, tt := range tts {
		require.Equal(t, tt.s, tt.r.String())

		ref, ok := parseRef(tt.s)
		require.True(t, ok)
		require.Equal(t, tt.r, ref)
	}
}
