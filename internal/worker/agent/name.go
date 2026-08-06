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

package agent

import (
	"fmt"
	"math/rand/v2"
)

// Readable node-name word lists: auto-generated names look like
// "proud-heron-7". The host module persists its auto-generated name; pool
// agents regenerate one on every run.
var nameAdjectives = []string{
	"proud", "swift", "gentle", "brave", "calm", "clever", "daring",
	"eager", "fancy", "golden", "happy", "jolly", "keen", "lively",
	"mellow", "nimble", "quiet", "silver", "witty", "zesty",
}

var nameAnimals = []string{
	"heron", "otter", "lynx", "falcon", "badger", "coyote", "dolphin",
	"eagle", "fox", "gecko", "hawk", "ibis", "jackal", "koala", "lemur",
	"marmot", "narwhal", "ocelot", "puma", "quokka",
}

// generateName builds a readable node name adjective-animal-N.
func generateName() string {
	return fmt.Sprintf("%s-%s-%d",
		nameAdjectives[rand.IntN(len(nameAdjectives))],
		nameAnimals[rand.IntN(len(nameAnimals))],
		rand.IntN(99)+1)
}
