package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseSyntheticGitlinks(t *testing.T) {
	input := `# libvirt's gnulib: old git:// URL and Savannah host, not convertible
gitlab.com/libvirt/libvirt   git://git.sv.gnu.org/gnulib.git
gitlab.com/libvirt/libvirt	path:.gnulib dir

*  https://git.savannah.gnu.org/git/gnulib.git/
`
	got, err := ParseSyntheticGitlinks(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	want := []SyntheticGitlink{
		{"gitlab.com/libvirt/libvirt", "git://git.sv.gnu.org/gnulib.git"},
		{"gitlab.com/libvirt/libvirt", "path:.gnulib dir"},
		{"*", "https://git.savannah.gnu.org/git/gnulib.git/"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}

	if e, ok := FindSyntheticApproval(got, "github.com/any/repo", "https://git.savannah.gnu.org/git/gnulib.git/"); !ok || e.String() != "* https://git.savannah.gnu.org/git/gnulib.git/" {
		t.Errorf("wildcard entry should approve any superproject, got %q, %v", e, ok)
	}
	if _, ok := FindSyntheticApproval(got, "github.com/other/repo", "git://git.sv.gnu.org/gnulib.git"); ok {
		t.Error("entry for libvirt should not approve another superproject")
	}
}

func TestParseSyntheticGitlinksRejectsMalformedLines(t *testing.T) {
	for _, line := range []string{"gitlab.com/libvirt/libvirt", "gitlab.com/libvirt/libvirt https://a.example/x.git extra"} {
		if _, err := ParseSyntheticGitlinks(strings.NewReader(line)); err == nil {
			t.Errorf("expected an error for %q", line)
		}
	}
}
