// Copyright 2015 The Cockroach Authors.
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

package config_test

import (
	"bytes"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/config"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
)

func TestDecodeObjectID(t *testing.T) {
	defer leaktest.AfterTest(t)()

	testCases := []struct {
		key       roachpb.RKey
		keySuffix []byte
		success   bool
		id        uint32
	}{
		// Before the structured span.
		{roachpb.RKeyMin, nil, false, 0},

		// Boundaries of structured span.
		{roachpb.RKeyMax, nil, false, 0},

		// Valid, even if there are things after the ID.
		{testutils.MakeKey(keys.MakeTablePrefix(42), roachpb.RKey("\xff")), []byte{'\xff'}, true, 42},
		{keys.MakeTablePrefix(0), []byte{}, true, 0},
		{keys.MakeTablePrefix(999), []byte{}, true, 999},
	}

	for tcNum, tc := range testCases {
		id, keySuffix, success := config.DecodeObjectID(tc.key)
		if success != tc.success {
			t.Errorf("#%d: expected success=%t", tcNum, tc.success)
			continue
		}
		if id != tc.id {
			t.Errorf("#%d: expected id=%d, got %d", tcNum, tc.id, id)
		}
		if !bytes.Equal(keySuffix, tc.keySuffix) {
			t.Errorf("#%d: expected suffix=%q, got %q", tcNum, tc.keySuffix, keySuffix)
		}
	}
}
