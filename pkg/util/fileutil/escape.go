// Copyright 2017 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License included
// in the file licenses/BSL.txt and at www.mariadb.com/bsl11.
//
// Change Date: 2022-10-01
//
// On the date above, in accordance with the Business Source License, use
// of this software will be governed by the Apache License, Version 2.0,
// included in the file licenses/APL.txt and at
// https://www.apache.org/licenses/LICENSE-2.0

package fileutil

import "regexp"

// EscapeFilename replaces bad characters in a filename with safe equivalents.
// The only character disallowed on Unix systems is the path separator "/".
// Windows is more restrictive; banned characters on Windows are listed here:
// https://msdn.microsoft.com/en-us/library/windows/desktop/aa365247(v=vs.85).aspx
func EscapeFilename(s string) string {
	return regexp.MustCompile(`[<>:"\/|?*\x00-\x1f]`).ReplaceAllString(s, "_")
}
