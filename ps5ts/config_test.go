package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseConfigExample(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "deploy", "ps5ts.conf.example"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := parseConfig(string(b))
	if err != nil {
		t.Fatal(err)
	}
	want := config{proxy: "http://192.168.0.10:3130", hostname: "ps5", ports: []int{9021}}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("got %+v, want %+v", cfg, want)
	}
}

// TestParseConfigFromTool parses what tools/ps5ts_config.py writes, so the
// two cannot drift apart. Skipped when python is not installed.
func TestParseConfigFromTool(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		if python, err = exec.LookPath("python"); err != nil {
			t.Skip("python not found")
		}
	}
	cases := []struct {
		args []string
		want config
	}{
		{[]string{"--proxy", "http://10.0.0.5:3130"}, config{proxy: "http://10.0.0.5:3130", hostname: "ps5", ports: []int{9021}}},
		{[]string{"--proxy", "", "--hostname", "console", "--forward", "9021, 2121"}, config{hostname: "console", ports: []int{9021, 2121}}},
	}
	for _, c := range cases {
		args := append([]string{"-I", filepath.Join("..", "tools", "ps5ts_config.py"), "console", "--host", "192.0.2.1", "--dry-run"}, c.args...)
		out, err := exec.Command(python, args...).Output()
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		// Drop the "# would write ..." banner the dry run prints first.
		_, text, _ := strings.Cut(string(out), "\n\n")
		cfg, err := parseConfig(text)
		if err != nil {
			t.Fatalf("%v: ps5ts rejects the tool's output: %v\n%s", c.args, err, text)
		}
		if !reflect.DeepEqual(cfg, c.want) {
			t.Fatalf("%v: got %+v, want %+v", c.args, cfg, c.want)
		}
	}
}

func TestParseConfigRejects(t *testing.T) {
	for _, text := range []string{
		"proxy = https://host:1\n",
		"proxy = host:3130\n",
		"hostname =\n",
		"forward = 80\n",
		"forward = 70000\n",
		"forward = x\n",
		"color = blue\n",
		"just words\n",
	} {
		if _, err := parseConfig(text); err == nil {
			t.Errorf("accepted %q", text)
		}
	}
}
