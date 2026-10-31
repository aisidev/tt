package cli

import (
	_ "embed"
	"fmt"
	"strings"
)

//go:embed templates/systemd-unit.tmpl
var systemdTemplate string

//go:embed templates/openrc-init.tmpl
var openRCTemplate string

//go:embed templates/remote-setup.sh.tmpl
var remoteTemplate string

// Render named Rust format fields while treating escaped braces and inserted
// values literally. Shell parameter expressions must not be re-interpreted.
func renderTemplate(template string, values map[string]string) string {
	var out strings.Builder
	for i := 0; i < len(template); {
		if i+1 < len(template) && template[i:i+2] == "{{" {
			out.WriteByte('{')
			i += 2
			continue
		}
		if i+1 < len(template) && template[i:i+2] == "}}" {
			out.WriteByte('}')
			i += 2
			continue
		}
		if template[i] == '{' {
			end := strings.IndexByte(template[i+1:], '}')
			if end < 0 {
				panic("invalid embedded template")
			}
			end += i + 1
			value, ok := values[template[i+1:end]]
			if !ok {
				panic("missing embedded template field: " + template[i+1:end])
			}
			out.WriteString(value)
			i = end + 1
			continue
		}
		out.WriteByte(template[i])
		i++
	}
	return out.String()
}
func systemdUnit(name, command string, root bool, environment *string) string {
	userLines := "User=ttstack\nGroup=ttstack"
	if root {
		userLines = "# VM processes must survive agent restarts\nKillMode=process\nTimeoutStopSec=360"
	}
	env := ""
	if environment != nil {
		env = "EnvironmentFile=" + *environment + "\n"
	}
	return renderTemplate(systemdTemplate, map[string]string{"name": name, "exec_start": command, "user_lines": userLines, "env_file_line": env})
}
func openRCInit(name, command string) string {
	program, args, found := strings.Cut(command, " ")
	if !found {
		args = ""
	}
	return renderTemplate(openRCTemplate, map[string]string{"name": name, "cmd": program, "args": args})
}

type envFile struct{ path, content string }

func remoteSetupScript(service, command, user, home, prefix, tmp string, binaries, extraDirs []string, label string, environment *envFile, ownedDirs ...string) string {
	copies := []string{}
	for _, binary := range binaries {
		copies = append(copies, fmt.Sprintf("sudo install -m 755 %s/%s %s/bin/.%s.new\nsudo mv -f %s/bin/.%s.new %s/bin/%s", tmp, binary, prefix, binary, prefix, binary, prefix, binary))
	}
	directories := append(append([]string{}, extraDirs...), home+"/data", home+"/run")
	var envPath *string
	if environment != nil {
		envPath = &environment.path
	}
	unit := systemdUnit(service, command, service == "tt-agent", envPath)
	unit = strings.ReplaceAll(unit, "User=ttstack", "User="+user)
	unit = strings.ReplaceAll(unit, "Group=ttstack", "Group="+user)
	init := openRCInit(service, command)
	if environment != nil {
		init += "\nstart_pre() {\n    . " + environment.path + "\n    export TT_API_KEY\n}\n"
	}
	if service == "tt-ctl" {
		init += "\ncommand_user=\"" + user + ":" + user + "\"\n"
	}
	setup, source := "", ""
	if environment != nil {
		path := environment.path
		setup = "\n# Write environment file (keeps secrets out of process args)\nsudo mkdir -p $(dirname " + path + ")\nsudo install -m 600 /dev/null " + path + "\nprintf '%s' '" + environment.content + "' | sudo tee " + path + " > /dev/null\nsudo chmod 600 " + path + "\n"
		source = ". " + path + " && export TT_API_KEY && "
	}
	ownership := ""
	if len(ownedDirs) > 0 {
		ownership = "\nsudo chown " + user + ":" + user + " " + strings.Join(ownedDirs, " ")
	}
	return renderTemplate(remoteTemplate, map[string]string{"user": user, "home": home, "prefix": prefix, "tmp": tmp, "extra_ownership": ownership, "dir_list": strings.Join(directories, " "), "bin_copies": strings.Join(copies, "\n"), "service_name": service, "exec_cmd": command, "systemd_unit": unit, "openrc_initd": init, "host_label": label, "env_file_setup": setup, "env_source": source})
}
func mountGuardDropin(unit string) *string {
	requires, checks := []string{}, []string{}
	for _, line := range strings.Split(unit, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "RequiresMountsFor=") {
			requires = append(requires, line)
		}
		if strings.HasPrefix(line, "ExecStartPre=") && strings.Contains(line, "mountpoint") {
			checks = append(checks, line)
		}
	}
	if len(requires) == 0 && len(checks) == 0 {
		return nil
	}
	value := "[Unit]\n" + strings.Join(requires, "\n") + "\n[Service]\n" + strings.Join(checks, "\n") + "\n"
	return &value
}
