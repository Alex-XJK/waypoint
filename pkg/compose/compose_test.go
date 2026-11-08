package compose

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// load writes yaml as compose.yaml in a fresh directory and loads it.
func load(t *testing.T, yaml string) (*Project, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := Load([]string{path}, "proj")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(p.Close)
	return p, dir
}

func TestLoadTranslatesServices(t *testing.T) {
	p, dir := load(t, `
x-waypoint-subnet: 10.77.0.0/24
services:
  web:
    x-waypoint-rootfs: ./rootfs
    depends_on: [api]
    command: python app.py --port 8000
    working_dir: /app
    environment:
      MODE: dev
      EMPTY: ""
  api:
    build:
      context: ./api
      dockerfile: Dockerfile.dev
      target: prod
      args: {VERSION: "2"}
    entrypoint: ["/entry.sh"]
    depends_on: [db]
  db:
    image: postgres:16
`)
	spec := p.Spec
	if spec.Name != "proj" || spec.Subnet != "10.77.0.0/24" {
		t.Fatalf("spec = %+v", spec)
	}
	var order []string
	for _, s := range spec.Services {
		order = append(order, s.Name)
	}
	if !reflect.DeepEqual(order, []string{"db", "api", "web"}) {
		t.Fatalf("order = %v, want dependencies first", order)
	}
	db, api, web := spec.Services[0], spec.Services[1], spec.Services[2]

	if web.Rootfs != filepath.Join(dir, "rootfs") || web.Build != nil {
		t.Fatalf("web source = %q %+v", web.Rootfs, web.Build)
	}
	if !reflect.DeepEqual(web.Command, []string{"python", "app.py", "--port", "8000"}) || web.Entrypoint != nil {
		t.Fatalf("web command = %q entrypoint = %q", web.Command, web.Entrypoint)
	}
	if web.WorkDir != "/app" || !reflect.DeepEqual(web.Env, []string{"EMPTY=", "MODE=dev"}) {
		t.Fatalf("web workdir = %q env = %q", web.WorkDir, web.Env)
	}

	b := api.Build
	if b == nil || b.Context != filepath.Join(dir, "api") || b.Dockerfile != "Dockerfile.dev" || b.Target != "prod" || b.Args["VERSION"] != "2" {
		t.Fatalf("api build = %+v", b)
	}
	if !reflect.DeepEqual(api.Entrypoint, []string{"/entry.sh"}) || api.Command != nil {
		t.Fatalf("api entrypoint = %q command = %q", api.Entrypoint, api.Command)
	}

	if db.Build == nil {
		t.Fatal("db has no generated build")
	}
	data, err := os.ReadFile(filepath.Join(db.Build.Context, "Dockerfile"))
	if err != nil || string(data) != "FROM postgres:16\n" {
		t.Fatalf("generated Dockerfile = %q, %v", data, err)
	}
	p.Close()
	if _, err := os.Stat(db.Build.Context); !os.IsNotExist(err) {
		t.Fatal("Close kept the generated build context")
	}
}

func TestLoadWarnsAboutIgnoredSettings(t *testing.T) {
	p, _ := load(t, `
services:
  web:
    image: nginx
    ports: ["8080:80"]
    volumes: ["data:/data"]
    restart: always
    depends_on:
      db: {condition: service_healthy}
  db:
    image: postgres
    healthcheck: {test: ["CMD", "true"]}
volumes:
  data: {}
networks:
  back: {}
`)
	all := strings.Join(p.Warnings, "\n")
	for _, want := range []string{
		"web: ports is ignored", "web: volumes is ignored", "web: restart is ignored",
		"db: healthcheck is ignored", "condition service_healthy is treated as service_started",
		"volumes: data is ignored", "networks: back is ignored",
	} {
		if !strings.Contains(all, want) {
			t.Fatalf("warnings lack %q:\n%s", want, all)
		}
	}
}

// A plain service must load without warnings: whatever compose-go fills in
// while normalizing is not something the user asked for.
func TestLoadIsQuietAboutSupportedSettings(t *testing.T) {
	p, _ := load(t, `
services:
  web:
    build: {context: ., dockerfile: Dockerfile, target: prod, args: {V: "1"}}
    command: serve
    entrypoint: ["/entry.sh"]
    working_dir: /app
    environment: {MODE: dev}
    labels: {tier: front}
    scale: 1
    init: false
    healthcheck: {disable: true}
    depends_on: [db]
  db:
    image: postgres
`)
	if len(p.Warnings) != 0 {
		t.Fatalf("unexpected warnings:\n%s", strings.Join(p.Warnings, "\n"))
	}
}

// Settings are warned about by exclusion, so ones no list names are caught.
func TestLoadWarnsAboutUnlistedSettings(t *testing.T) {
	p, _ := load(t, `
services:
  web:
    build: {context: ., network: host}
    container_name: front
    shm_size: 64m
    mem_limit: 1g
`)
	all := strings.Join(p.Warnings, "\n")
	for _, want := range []string{
		"web: container_name is ignored", "web: shm_size is ignored",
		"web: mem_limit is ignored", "web: build.network is ignored",
	} {
		if !strings.Contains(all, want) {
			t.Fatalf("warnings lack %q:\n%s", want, all)
		}
	}
}

func TestLoadRejects(t *testing.T) {
	for want, yaml := range map[string]string{
		"needs image, build, or": "services:\n  a:\n    command: echo hi\n",
		"cycle":                  "services:\n  a:\n    image: x\n    depends_on: [b]\n  b:\n    image: x\n    depends_on: [a]\n",
		"undefined service":      "services:\n  a:\n    image: x\n    depends_on: [nope]\n",
		"must be a string":       "services:\n  a:\n    x-waypoint-rootfs: 3\n",
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "compose.yaml")
		if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		p, err := Load([]string{path}, "proj")
		if err == nil {
			p.Close()
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Load(%q) = %v, want error containing %q", yaml, err, want)
		}
	}
}
