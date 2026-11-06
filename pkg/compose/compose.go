// Package compose translates a compose file into a group.Spec. Only this
// package imports compose-go: it parses and normalizes the file (variable
// interpolation, .env and env_file, extends, profiles) and maps the subset
// Waypoint supports onto sessions; everything else is reported as a warning
// and ignored.
package compose

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/types"

	"github.com/Alex-XJK/waypoint/pkg/group"
	"github.com/Alex-XJK/waypoint/pkg/waypoint"
)

// RootfsExtension names a directory to use as a service's rootfs, as for
// `waypoint init`, instead of building an image.
const RootfsExtension = "x-waypoint-rootfs"

// SubnetExtension, at the top level, sets the project's fabric subnet.
const SubnetExtension = "x-waypoint-subnet"

// Project is a loaded compose file.
type Project struct {
	Spec     group.Spec
	Warnings []string
	// tempDirs hold generated build contexts for `image:` services.
	tempDirs []string
}

// Close removes the generated build contexts; call it once Up is done.
func (p *Project) Close() {
	for _, d := range p.tempDirs {
		_ = os.RemoveAll(d)
	}
}

// Load reads the compose files (the first is primary, later ones override;
// none means the default file in the working directory). name, if not empty,
// overrides the project name compose would infer.
func Load(files []string, name string) (*Project, error) {
	// compose-go's consistency check refuses a service with neither image nor
	// build, which is what an x-waypoint-rootfs service is; translate checks
	// what Waypoint needs (a source per service, known dependencies).
	opts := []cli.ProjectOptionsFn{cli.WithOsEnv, cli.WithDotEnv, cli.WithConsistency(false)}
	if len(files) == 0 {
		// compose.yaml, compose.yml, docker-compose.yaml, ... in the cwd.
		opts = append(opts, cli.WithDefaultConfigPath)
	}
	if name != "" {
		opts = append(opts, cli.WithName(name))
	}
	options, err := cli.NewProjectOptions(files, opts...)
	if err != nil {
		return nil, err
	}
	project, err := options.LoadProject(context.Background())
	if err != nil {
		return nil, err
	}
	return translate(project)
}

func translate(project *types.Project) (_ *Project, err error) {
	p := &Project{Spec: group.Spec{Name: project.Name}}
	defer func() {
		if err != nil {
			p.Close() // the generated build contexts made so far
		}
	}()
	if v, ok := project.Extensions[SubnetExtension]; ok {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s must be a string", SubnetExtension)
		}
		p.Spec.Subnet = s
	}
	for _, n := range sortedKeys(project.Networks) {
		if n != "default" {
			p.warn("networks: %s is ignored; every service shares one network", n)
		}
	}
	for _, v := range sortedKeys(project.Volumes) {
		p.warn("volumes: %s is ignored", v)
	}

	order, err := dependencyOrder(project.Services)
	if err != nil {
		return nil, err
	}
	for _, name := range order {
		svc, err := p.service(project, project.Services[name])
		if err != nil {
			return nil, fmt.Errorf("service %s: %w", name, err)
		}
		p.Spec.Services = append(p.Spec.Services, svc)
	}
	return p, nil
}

func (p *Project) service(project *types.Project, s types.ServiceConfig) (group.Service, error) {
	svc := group.Service{
		Name:       s.Name,
		Entrypoint: []string(s.Entrypoint),
		Command:    []string(s.Command),
		WorkDir:    s.WorkingDir,
		Env:        environment(s.Environment),
	}
	if v, ok := s.Extensions[RootfsExtension]; ok {
		dir, ok := v.(string)
		if !ok {
			return svc, fmt.Errorf("%s must be a string", RootfsExtension)
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(project.WorkingDir, dir)
		}
		svc.Rootfs = dir
	} else if s.Build != nil {
		if s.Build.DockerfileInline != "" {
			return svc, fmt.Errorf("build.dockerfile_inline is not supported")
		}
		svc.Build = &waypoint.BuildSpec{
			Context:    s.Build.Context,
			Dockerfile: s.Build.Dockerfile,
			Args:       buildArgs(s.Build.Args),
			Target:     s.Build.Target,
		}
	} else if s.Image != "" {
		dir, err := os.MkdirTemp("", "waypoint-image-")
		if err != nil {
			return svc, err
		}
		p.tempDirs = append(p.tempDirs, dir)
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM "+s.Image+"\n"), 0o644); err != nil {
			return svc, err
		}
		svc.Build = &waypoint.BuildSpec{Context: dir}
	} else {
		return svc, fmt.Errorf("needs image, build, or %s", RootfsExtension)
	}
	p.unsupported(s)
	return svc, nil
}

// unsupported warns about the service settings Waypoint ignores.
func (p *Project) unsupported(s types.ServiceConfig) {
	ignored := map[string]bool{
		"ports":             len(s.Ports) > 0,
		"expose":            len(s.Expose) > 0,
		"volumes":           len(s.Volumes) > 0 || len(s.VolumesFrom) > 0,
		"healthcheck":       s.HealthCheck != nil && !s.HealthCheck.Disable,
		"restart":           s.Restart != "",
		"deploy":            s.Deploy != nil,
		"scale":             s.Scale != nil && *s.Scale != 1,
		"user":              s.User != "",
		"hostname":          s.Hostname != "",
		"extra_hosts":       len(s.ExtraHosts) > 0,
		"dns":               len(s.DNS) > 0 || len(s.DNSSearch) > 0 || len(s.DNSOpts) > 0,
		"network_mode":      s.NetworkMode != "",
		"cap_add":           len(s.CapAdd) > 0 || len(s.CapDrop) > 0,
		"privileged":        s.Privileged,
		"devices":           len(s.Devices) > 0,
		"secrets":           len(s.Secrets) > 0,
		"configs":           len(s.Configs) > 0,
		"tmpfs":             len(s.Tmpfs) > 0,
		"sysctls":           len(s.Sysctls) > 0,
		"ulimits":           len(s.Ulimits) > 0,
		"limits":            s.MemLimit != 0 || s.CPUS != 0 || s.PidsLimit != 0,
		"init":              s.Init != nil && *s.Init,
		"tty":               s.Tty || s.StdinOpen,
		"build.secrets/ssh": s.Build != nil && (len(s.Build.Secrets) > 0 || len(s.Build.SSH) > 0),
	}
	for _, key := range sortedKeys(ignored) {
		if ignored[key] {
			p.warn("service %s: %s is ignored", s.Name, key)
		}
	}
	for _, dep := range sortedKeys(s.DependsOn) {
		if c := s.DependsOn[dep].Condition; c != "" && c != types.ServiceConditionStarted {
			p.warn("service %s: depends_on %s condition %s is treated as service_started", s.Name, dep, c)
		}
	}
	for _, n := range sortedKeys(s.Networks) {
		if net := s.Networks[n]; net != nil && (len(net.Aliases) > 0 || net.Ipv4Address != "") {
			p.warn("service %s: network aliases and addresses are ignored", s.Name)
			break
		}
	}
}

func (p *Project) warn(format string, args ...any) {
	p.Warnings = append(p.Warnings, fmt.Sprintf(format, args...))
}

// dependencyOrder sorts services so each follows those it depends on; ties
// go alphabetically.
func dependencyOrder(services types.Services) ([]string, error) {
	pending := map[string]int{}
	dependents := map[string][]string{}
	for name, s := range services {
		pending[name] += 0
		for dep, d := range s.DependsOn {
			if _, ok := services[dep]; !ok {
				if d.Required {
					return nil, fmt.Errorf("service %s depends on undefined service %s", name, dep)
				}
				continue
			}
			pending[name]++
			dependents[dep] = append(dependents[dep], name)
		}
	}
	var ready, order []string
	for name, n := range pending {
		if n == 0 {
			ready = append(ready, name)
		}
	}
	for len(ready) > 0 {
		sort.Strings(ready)
		name := ready[0]
		ready = ready[1:]
		order = append(order, name)
		for _, d := range dependents[name] {
			if pending[d]--; pending[d] == 0 {
				ready = append(ready, d)
			}
		}
	}
	if len(order) != len(services) {
		return nil, fmt.Errorf("depends_on has a cycle")
	}
	return order, nil
}

// environment renders a service's environment as sorted KEY=VALUE entries;
// a variable compose left unset (KEY with no value and none in the
// environment) is dropped.
func environment(env types.MappingWithEquals) []string {
	var out []string
	for _, k := range sortedKeys(env) {
		if v := env[k]; v != nil {
			out = append(out, k+"="+*v)
		}
	}
	return out
}

func buildArgs(args types.MappingWithEquals) map[string]string {
	if len(args) == 0 {
		return nil
	}
	out := make(map[string]string, len(args))
	for k, v := range args {
		if v != nil {
			out[k] = *v
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// DescribeService summarizes how a service's rootfs is made, for messages.
func DescribeService(s group.Service) string {
	switch {
	case s.Rootfs != "":
		return "rootfs " + s.Rootfs
	case s.Build != nil:
		return "build " + strings.TrimSpace(s.Build.Context)
	}
	return "?"
}
