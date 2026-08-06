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

package detect

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// TestParseDotfileSingleFile covers the full schema of one dotfile body.
func TestParseDotfileSingleFile(t *testing.T) {
	data := []byte(`maintainers = ["alice@example.org"]

vcs = "git"

[pkgbuild_source]
url = "git@git.example.org:other.git"
branch = "master"
directory = "pkgs/foo"

[collect]
exclude = ["*-debug"]

[hooks]
pre_build = ["scripts/pre.sh"]
post_build = ["scripts/post.sh"]
on_success = ["scripts/ok.sh"]
on_failure = ["scripts/fail.sh"]
`)
	d, err := ParseDotfile(data)
	if err != nil {
		t.Fatalf("ParseDotfile: %v", err)
	}
	if !reflect.DeepEqual(d.Maintainers, []string{"alice@example.org"}) {
		t.Errorf("Maintainers = %v", d.Maintainers)
	}
	if d.VCS != "git" {
		t.Errorf("VCS = %q, want git", d.VCS)
	}
	wantSrc := &PkgbuildSource{URL: "git@git.example.org:other.git", Branch: "master", Directory: "pkgs/foo"}
	if !reflect.DeepEqual(d.PkgbuildSource, wantSrc) {
		t.Errorf("PkgbuildSource = %+v, want %+v", d.PkgbuildSource, wantSrc)
	}
	if !reflect.DeepEqual(d.Collect.Exclude, []string{"*-debug"}) {
		t.Errorf("Collect.Exclude = %v", d.Collect.Exclude)
	}
	wantHooks := Hooks{
		PreBuild:  []string{"scripts/pre.sh"},
		PostBuild: []string{"scripts/post.sh"},
		OnSuccess: []string{"scripts/ok.sh"},
		OnFailure: []string{"scripts/fail.sh"},
	}
	if !reflect.DeepEqual(d.Hooks, wantHooks) {
		t.Errorf("Hooks = %+v, want %+v", d.Hooks, wantHooks)
	}
}

// TestParseDotfileEmpty treats an empty body as an empty Dotfile (a branch
// without a dotfile builds as a plain PKGBUILD branch).
func TestParseDotfileEmpty(t *testing.T) {
	d, err := ParseDotfile(nil)
	if err != nil {
		t.Fatalf("ParseDotfile(nil): %v", err)
	}
	if d.VCS != "" || len(d.Maintainers) != 0 || d.PkgbuildSource != nil {
		t.Errorf("empty dotfile = %+v, want zero value", d)
	}
}

// TestParseDotfileInvalid asserts invalid TOML is an error.
func TestParseDotfileInvalid(t *testing.T) {
	for _, data := range []string{"not toml [[[", "maintainers = 42", "maintainers = [", "vcs = [\"git\"]"} {
		if _, err := ParseDotfile([]byte(data)); err == nil {
			t.Errorf("ParseDotfile(%q) succeeded, want error", data)
		}
	}
}

// TestDotfileMerge covers the merge semantics across extras: maintainers/
// hooks append, vcs and pkgbuild_source are overridden by later files,
// collect.exclude appends de-duplicated.
func TestDotfileMerge(t *testing.T) {
	files := map[string]string{
		"main": `maintainers = ["a@example.org"]
vcs = "auto"
extras = ["extra1.toml", "extra2.toml"]
[pkgbuild_source]
url = "git@x:y.git"
branch = "master"
directory = "pkgs/foo"
[collect]
exclude = ["*-debug"]
[hooks]
pre_build = ["scripts/pre.sh"]
`,
		"extra1.toml": `maintainers = ["b@example.org"]
vcs = "git"
[collect]
exclude = ["*-debug", "extra"]
[hooks]
post_build = ["scripts/post.sh"]
extras = ["extra2.toml"]
`,
		"extra2.toml": `maintainers = ["c@example.org"]
[pkgbuild_source]
url = "git@x:z.git"
[hooks]
on_success = ["scripts/ok.sh"]
`,
	}
	get := mapGetter(files)
	d, err := ParseDotfileWithExtras(get, []byte(files["main"]))
	if err != nil {
		t.Fatalf("ParseDotfileWithExtras: %v", err)
	}

	if !reflect.DeepEqual(d.Maintainers, []string{"a@example.org", "b@example.org", "c@example.org"}) {
		t.Errorf("Maintainers = %v", d.Maintainers)
	}
	if d.VCS != "git" {
		t.Errorf("VCS = %q, want git (extra1 overrides main)", d.VCS)
	}
	wantSrc := &PkgbuildSource{URL: "git@x:z.git"}
	if !reflect.DeepEqual(d.PkgbuildSource, wantSrc) {
		t.Errorf("PkgbuildSource = %+v, want %+v (extra2 overrides main)", d.PkgbuildSource, wantSrc)
	}
	if !reflect.DeepEqual(d.Collect.Exclude, []string{"*-debug", "extra"}) {
		t.Errorf("Collect.Exclude = %v, want [*-debug extra] de-duplicated", d.Collect.Exclude)
	}
	wantHooks := Hooks{
		PreBuild:  []string{"scripts/pre.sh"},
		PostBuild: []string{"scripts/post.sh"},
		OnSuccess: []string{"scripts/ok.sh"},
	}
	if !reflect.DeepEqual(d.Hooks, wantHooks) {
		t.Errorf("Hooks = %+v, want %+v", d.Hooks, wantHooks)
	}
}

// TestDotfileCycle asserts that an extras cycle A -> B -> A is an error.
func TestDotfileCycle(t *testing.T) {
	files := map[string]string{
		"a.toml": "extras = [\"b.toml\"]\n",
		"b.toml": "extras = [\"a.toml\"]\n",
	}
	if _, err := ParseDotfileWithExtras(mapGetter(files), []byte(files["a.toml"])); err == nil {
		t.Error("ParseDotfileWithExtras with cycle succeeded, want error")
	}
}

// TestDotfileDepth asserts the recursion limit: depth 8 is fine, depth 9
// errors. The main file counts as depth 1.
func TestDotfileDepth(t *testing.T) {
	// Chain of 7 extras: the deepest file sits at depth 8, which is allowed.
	files := map[string]string{}
	for i := 0; i < 7; i++ {
		files[itof(i)] = `extras = ["` + itof(i+1) + `"]` + "\n"
	}
	files[itof(7)] = "" // chain terminator at depth 8
	if _, err := ParseDotfileWithExtras(mapGetter(files), []byte(files["0"])); err != nil {
		t.Fatalf("depth 8 chain errored: %v", err)
	}
	// Chain of 8 extras: the last file sits at depth 9, which must error.
	files[itof(7)] = `extras = ["` + itof(8) + `"]` + "\n"
	files[itof(8)] = ""
	if _, err := ParseDotfileWithExtras(mapGetter(files), []byte(files["0"])); err == nil {
		t.Error("depth 9 chain succeeded, want error")
	}
}

// itof renders a small integer as a hex string so the depth test keys are
// as short as the chained extras.
func itof(i int) string {
	const digits = "0123456789abcdef"
	return string(digits[i])
}

// TestDotfileMissingExtra asserts that a referenced extras file that get
// cannot resolve is an error.
func TestDotfileMissingExtra(t *testing.T) {
	files := map[string]string{
		"main": "extras = [\"gone.toml\"]\n",
	}
	if _, err := ParseDotfileWithExtras(mapGetter(files), []byte(files["main"])); err == nil {
		t.Error("ParseDotfileWithExtras with missing extra succeeded, want error")
	}
}

// TestDotfileNilGet asserts the precondition that get must be non-nil.
func TestDotfileNilGet(t *testing.T) {
	if _, err := ParseDotfileWithExtras(nil, []byte("maintainers = [\"a@example.org\"]\n")); err == nil {
		t.Error("ParseDotfileWithExtras(nil get) succeeded, want error")
	}
}

// TestDotfileUnknownKeys asserts unknown TOML keys are ignored for forward
// compatibility.
func TestDotfileUnknownKeys(t *testing.T) {
	data := []byte("maintainers = [\"a@example.org\"]\nfuture_field = \"x\"\n[future_table]\nx = 1\n")
	d, err := ParseDotfile(data)
	if err != nil {
		t.Fatalf("ParseDotfile with unknown keys: %v", err)
	}
	if !reflect.DeepEqual(d.Maintainers, []string{"a@example.org"}) {
		t.Errorf("Maintainers = %v", d.Maintainers)
	}
}

// mapGetter adapts a path -> content map to the get callback.
func mapGetter(files map[string]string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		data, ok := files[path]
		if !ok {
			return nil, os.ErrNotExist
		}
		return []byte(data), nil
	}
}

// TestDotfileMaintainerOrder is a sanity check that sibling extras merge in
// listed order even when one of them recursively pulls more files.
func TestDotfileMaintainerOrder(t *testing.T) {
	files := map[string]string{
		"main":     "maintainers = [\"m@example.org\"]\nextras = [\"s1.toml\", \"s2.toml\"]\n",
		"s1.toml":  "maintainers = [\"one@example.org\"]\nextras = [\"s1a.toml\"]\n",
		"s1a.toml": "maintainers = [\"one-a@example.org\"]\n",
		"s2.toml":  "maintainers = [\"two@example.org\"]\n",
	}
	d, err := ParseDotfileWithExtras(mapGetter(files), []byte(files["main"]))
	if err != nil {
		t.Fatalf("ParseDotfileWithExtras: %v", err)
	}
	want := "m@example.org one@example.org one-a@example.org two@example.org"
	if got := strings.Join(d.Maintainers, " "); got != want {
		t.Errorf("Maintainers order = %q, want %q", got, want)
	}
}
