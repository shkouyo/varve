// SPDX-License-Identifier: AGPL-3.0-or-later

// Copyright (C) 2026 ShinKouyo <i@0x0f.dev>
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// warnW is the sink for non-fatal configuration warnings; it defaults to
// os.Stderr. Tests may replace it to capture warning output.
var warnW io.Writer = os.Stderr

// parseDotenv parses dotenv-style content.
//
// Supported syntax:
//   - KEY=VALUE lines (the value is trimmed of surrounding whitespace)
//   - "double" and 'single' quoted values (quotes are stripped)
//   - "# comment" lines and blank lines (skipped)
//
// Variable interpolation is not performed. Lines that do not match the
// syntax are skipped with a warning written to warn; they never abort the
// caller.
func parseDotenv(data []byte, warn io.Writer) map[string]string {
	env := make(map[string]string)
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			fmt.Fprintf(warn, "varve: warning: .env: ignoring line without '=': %q\n", line)
			continue
		}
		key := strings.TrimSpace(line[:eq])
		if key == "" {
			fmt.Fprintf(warn, "varve: warning: .env: ignoring line with empty key: %q\n", line)
			continue
		}
		val := strings.TrimSpace(line[eq+1:])
		if len(val) >= 2 {
			first, last := val[0], val[len(val)-1]
			if (first == '"' || first == '\'') && last == first {
				val = val[1 : len(val)-1]
			} else if first == '"' || first == '\'' {
				fmt.Fprintf(warn, "varve: warning: .env: ignoring line with unterminated quote: %q\n", line)
				continue
			}
		}
		env[key] = val
	}
	return env
}

// loadDotenvFile loads a dotenv file from path. A missing file is not an
// error; read failures and syntax warnings are reported on warnW and do not
// abort the caller.
func loadDotenvFile(path string) map[string]string {
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(warnW, "varve: warning: %s: %v\n", path, err)
		}
		return map[string]string{}
	}
	return parseDotenv(data, warnW)
}
