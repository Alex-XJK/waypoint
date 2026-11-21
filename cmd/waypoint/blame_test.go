package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseBlameArgs(t *testing.T) {
	for _, tc := range []struct {
		in      []string
		want    blameArgs
		wantErr string
	}{
		{in: []string{"/app/a"}, want: blameArgs{paths: []string{"/app/a"}}},
		{in: []string{"/a", "--lines", "/b"}, want: blameArgs{paths: []string{"/a", "/b"}, lines: true}},
		{in: []string{"--paths-from", "-"}, want: blameArgs{pathsFrom: "-"}},
		{in: []string{"--lines", "--paths-from", "list.txt", "/c"}, want: blameArgs{paths: []string{"/c"}, lines: true, pathsFrom: "list.txt"}},
		{in: []string{"--lines"}, wantErr: "no path"},
		{in: []string{"/a", "--paths-from"}, wantErr: "requires a value"},
		{in: []string{"--paths-from", "x", "--paths-from", "y"}, wantErr: "twice"},
		{in: []string{"/a", "--dir"}, wantErr: "unknown flag"},
	} {
		got, err := parseBlameArgs(tc.in)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("parseBlameArgs(%q) error = %v, want %q", tc.in, err, tc.wantErr)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseBlameArgs(%q) = %+v, %v; want %+v", tc.in, got, err, tc.want)
		}
	}
}

func TestReadBlamePaths(t *testing.T) {
	got, err := readBlamePaths(strings.NewReader("/a\n\n  \n/b c\r\n/d"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/a", "/b c", "/d"}; !reflect.DeepEqual(got, want) {
		t.Errorf("readBlamePaths = %q, want %q", got, want)
	}
}
