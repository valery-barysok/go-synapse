package synapse

import (
	"text/template"
	"github.com/n0rad/go-erlog/errs"
	"github.com/n0rad/go-erlog/data"
	"bufio"
	"bytes"
	"io/ioutil"
	"github.com/n0rad/go-erlog/logs"
	"github.com/blablacar/go-nerve/nerve"
	"os"
	"time"
	"net"
	"regexp"
	"strings"
)

const haProxyConfigurationTemplate = `# Handled by synapse. Do not modify it.
global
{{- range .Global}}
  {{.}}{{end}}

defaults
{{- range .Defaults}}
  {{.}}{{end}}

{{range $key, $element := .Listen}}
listen {{$key}}
{{- range $element}}
  {{.}}{{end}}
{{end}}
{{range $key, $element := .Frontend}}
frontend {{$key}}
{{- range $element}}
  {{.}}{{end}}
{{end}}
{{range $key, $element := .Backend}}
backend {{$key}}
{{- range $element}}
  {{.}}{{end}}
{{end}}

`

type HaProxyConfig struct {
	Global   []string
	Defaults []string
	Listen   map[string][]string
	Frontend map[string][]string
	Backend  map[string][]string
}

type HaProxyClient struct {
	HaProxyConfig
	ConfigPath               string
	ReloadCommand            []string
	ReloadMinIntervalInMilli int
	ReloadTimeoutInMilli     int
	StatePath                string

	socketPath               string
	weightRegex              *regexp.Regexp
	lastReload               time.Time
	template                 *template.Template
	fields                   data.Fields
}

func (hap *HaProxyClient) Init() error {
	hap.fields = data.WithField("config", hap.ConfigPath)

	if hap.Listen == nil {
		hap.Listen = make(map[string][]string)
	}
	if hap.Frontend == nil {
		hap.Frontend = make(map[string][]string)
	}
	if hap.Backend == nil {
		hap.Backend = make(map[string][]string)
	}

	if hap.ReloadMinIntervalInMilli == 0 {
		hap.ReloadMinIntervalInMilli = 500
	}

	if hap.ReloadTimeoutInMilli == 0 {
		hap.ReloadTimeoutInMilli = 1000
	}

	hap.socketPath = hap.findSocketPath()
	if hap.socketPath == "" {
		logs.WithF(hap.fields).Warn("No socketPath file specified. Will update by reload only")
	}

	hap.weightRegex = regexp.MustCompile(`weight ([0-9]+)`)
	tmpl, err := template.New("ha-proxy-config").Parse(haProxyConfigurationTemplate)
	if err != nil {
		return errs.WithEF(err, hap.fields, "Failed to parse haproxy config template")
	}
	hap.template = tmpl

	return nil
}

func (hap *HaProxyClient) findSocketPath() string {
	for _, str := range hap.Global {
		if strings.HasPrefix(str, "stats socket ") {
			start := len("stats socket ")
			return strings.TrimSpace(str[start:start + strings.Index(str[start:], " ")])
		}
	}
	return ""
}

func (hap *HaProxyClient) Reload() error {
	if err := hap.writeConfig(); err != nil {
		return errs.WithEF(err, hap.fields, "Failed to write haproxy configuration")
	}

	logs.WithF(hap.fields).Debug("Reloading haproxy")
	env := append(os.Environ(), "HAP_CONFIG=" + hap.ConfigPath)

	waitDuration := hap.lastReload.Add(time.Duration(hap.ReloadMinIntervalInMilli) * time.Millisecond).Sub(time.Now())
	if waitDuration > 0 {
		logs.WithF(hap.fields.WithField("wait", waitDuration)).Debug("Reloading too fast")
		time.Sleep(waitDuration)
	}
	defer func() {
		hap.lastReload = time.Now()
	}()
	if err := nerve.ExecCommandFull(hap.ReloadCommand, env, hap.ReloadTimeoutInMilli); err != nil {
		return errs.WithEF(err, hap.fields, "Failed to reload haproxy")
	}
	return nil
}

func (hap *HaProxyClient) SocketUpdate() error {
	if hap.socketPath == "" {
		return errs.WithF(hap.fields, "No socket file specified. Cannot update")
	}
	logs.WithF(hap.fields).Debug("Updating haproxy by socket")

	conn, err := net.Dial("unix", hap.socketPath)
	if err != nil {
		return errs.WithEF(err, hap.fields, "Failed to connect to haproxy socket")
	}
	defer conn.Close()

	i := 0
	b := bytes.Buffer{}
	for name, servers := range hap.Backend {
		for _, server := range servers {
			res := hap.weightRegex.FindStringSubmatch(server)
			serverInfo := strings.Split(server, " ")
			if len(res) > 0 {
				i++
				b.WriteString("set weight " + name + "/" + serverInfo[1] + " " + res[1] + "\n")
			}
		}

	}

	commands := b.Bytes()
	count, err := conn.Write(commands)
	if count != len(commands) || err != nil {
		return errs.WithEF(err, hap.fields.
		WithField("written", count).
		WithField("len", len(commands)).
		WithField("command", string(commands)), "Failed to write command to haproxy")
	}

	buff := bufio.NewReader(conn)
	line, prefix, err := buff.ReadLine()
	if err != nil || prefix {
		return errs.WithEF(err, hap.fields.WithField("line-too-long", prefix), "Failed to read hap socket response")
	}
	if string(line) != "" {
		return errs.WithF(hap.fields.WithField("response", string(line)), "Bad response for haproxy socket command")
	}

	return nil
}

func (hap *HaProxyClient) writeConfig() error {
	var b bytes.Buffer
	writer := bufio.NewWriter(&b)
	if err := hap.template.Execute(writer, hap); err != nil {
		return errs.WithEF(err, hap.fields, "Failed to temlate haproxy configuration file")
	}
	if err := writer.Flush(); err != nil {
		return errs.WithEF(err, hap.fields, "Failed to flush buffer")
	}

	templated := b.Bytes()
	if logs.IsTraceEnabled() {
		logs.WithF(hap.fields.WithField("templated", string(templated))).Trace("Templated configuration file")
	}
	if err := ioutil.WriteFile(hap.ConfigPath, templated, 0644); err != nil {
		return errs.WithEF(err, hap.fields, "Failed to write configuration file")
	}
	return nil
}

