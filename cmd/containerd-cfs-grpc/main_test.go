// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"strings"
	"testing"
)

// TestParseAdmit covers the -admit values: any and preloaded select the two
// modes, and anything else, including an empty value, is rejected with an
// error naming both.
func TestParseAdmit(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    bool
		wantErr bool
	}{
		{value: "any", want: true},
		{value: "preloaded", want: false},
		{value: "", wantErr: true},
		{value: "true", wantErr: true},
	} {
		got, err := parseAdmit(tc.value)
		if tc.wantErr {
			if err == nil || !strings.Contains(err.Error(), "want any or preloaded") {
				t.Errorf("parseAdmit(%q) error = %v, want one naming any and preloaded", tc.value, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("parseAdmit(%q) = %v, %v; want %v, nil", tc.value, got, err, tc.want)
		}
	}
}

// TestAdmitFlag covers the flag surface: -admit defaults to any, and the
// removed -allow-unstaged-pulls is no longer defined, so the flag package
// rejects it at startup.
func TestAdmitFlag(t *testing.T) {
	f := flag.Lookup("admit")
	if f == nil || f.DefValue != "any" {
		t.Fatalf("-admit flag = %+v, want default any", f)
	}
	if flag.Lookup("allow-unstaged-pulls") != nil {
		t.Fatal("-allow-unstaged-pulls is still defined")
	}
}
