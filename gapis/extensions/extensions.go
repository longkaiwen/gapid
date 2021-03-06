// Copyright (C) 2017 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package extensions provides extension functionality to GAPIS.
//
// Extensions would ideally be plugins, but golang still doesn't have
// cross platform support. See: https://github.com/golang/go/issues/19282
package extensions

import (
	"sync"

	"github.com/google/gapid/gapis/resolve/cmdgrouper"
)

var (
	extensions []Extension
	mutex      sync.Mutex
)

// Extension is a GAPIS extension.
// It should be registered at application initialization with Register.
type Extension struct {
	// Name of the extension.
	Name string
	// Custom command groupers.
	CmdGroupers func() []cmdgrouper.Grouper
}

// Register registers the extension e.
func Register(e Extension) {
	mutex.Lock()
	defer mutex.Unlock()

	extensions = append(extensions, e)
}

// Get returns the full list of registered extensions.
func Get() []Extension {
	mutex.Lock()
	defer mutex.Unlock()

	out := make([]Extension, len(extensions))
	copy(out, extensions)
	return out
}
