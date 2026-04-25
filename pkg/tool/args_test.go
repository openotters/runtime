//nolint:testpackage // tests the unexported splitArgs parser
package tool

import (
	"reflect"
	"testing"
)

func TestSplitArgs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"whitespace only", "   \t  ", nil},
		{"single token", "ls", []string{"ls"}},
		{"multiple tokens", "ls -la /tmp", []string{"ls", "-la", "/tmp"}},
		{"tabs separate", "a\tb\tc", []string{"a", "b", "c"}},
		{"double-quoted", `echo "hello world"`, []string{"echo", "hello world"}},
		{"single-quoted", `echo 'hello world'`, []string{"echo", "hello world"}},
		{"mixed quotes", `echo "a b" 'c d'`, []string{"echo", "a b", "c d"}},
		{"quote drops", `"abc"`, []string{"abc"}},
		{"adjacent quoted+unquoted", `pre"in quoted"post`, []string{"prein quotedpost"}},
		{"trailing whitespace", "ls  ", []string{"ls"}},
		{"leading whitespace", "   ls", []string{"ls"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := splitArgs(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("splitArgs(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}
