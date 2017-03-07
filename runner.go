package main

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"text/template"
	"time"
        "regexp"
	"github.com/fatih/structs"

	log "github.com/Sirupsen/logrus"
	"github.com/rancher/go-rancher-metadata/metadata"
)

var (
	MetadataURL = "http://rancher-metadata"
)

type runner struct {
	Config  *Config
	Client  metadata.Client
	Version string

	quitChan chan os.Signal
}

func NewRunner(conf *Config) (*runner, error) {
	u, _ := url.Parse(MetadataURL)
	u.Path = path.Join(u.Path, conf.MetadataVersion)

	log.Infof("Initializing Rancher Metadata client (version %s)", conf.MetadataVersion)

	client, err := metadata.NewClientAndWait(u.String())
	if err != nil {
		return nil, fmt.Errorf("Failed to initialize Rancher Metadata client: %v", err)
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	return &runner{
		Config:   conf,
		Client:   client,
		Version:  "init",
		quitChan: c,
	}, nil
}

func (r *runner) Run() error {
	if r.Config.OneTime {
		log.Info("Processing all templates once.")
		return r.poll()
	}

	log.Info("Polling Metadata with %d second interval", r.Config.Interval)
	ticker := time.NewTicker(time.Duration(r.Config.Interval) * time.Second)
	defer ticker.Stop()
	for {
		if err := r.poll(); err != nil {
			log.Error(err)
		}

		select {
		case <-ticker.C:
		case signal := <-r.quitChan:
			log.Info("Exit requested by signal: ", signal)
			return nil
		}
	}
}

func (r *runner) poll() error {
	log.Debug("Checking for metadata change")
	newVersion, err := r.Client.GetVersion()
	if err != nil {
		time.Sleep(time.Second * 2)
		return fmt.Errorf("Failed to get Metadata version: %v", err)
	}

	if r.Version == newVersion {
		log.Debug("No changes in Metadata")
		return nil
	}

	log.Debugf("Old version: %s, New Version: %s", r.Version, newVersion)

	r.Version = newVersion
	ctx, err := r.createContext()
	if err != nil {
		time.Sleep(time.Second * 2)
		return fmt.Errorf("Failed to create context from Rancher Metadata: %v", err)
	}

	tmplFuncs := newFuncMap(ctx)
	for _, tmpl := range r.Config.Templates {
		if err := r.processTemplate(tmplFuncs, tmpl); err != nil {
			return err
		}
	}

	if r.Config.OneTime {
		log.Info("All templates processed. Exiting.")
	} else {
		log.Info("All templates processed. Waiting for changes in Metadata...")
	}

	return nil
}

func (r *runner) processTemplate(funcs template.FuncMap, t Template) error {

	if (t.Source != "") && (t.Dest != "") {
		log.Debugf("Processing template %s for destination %s", t.Source, t.Dest)
		if _, err := os.Stat(t.Source); os.IsNotExist(err) {
			log.Fatalf("Template '%s' is missing", t.Source)
		}

		tmplBytes, err := ioutil.ReadFile(t.Source)
		if err != nil {
			log.Fatalf("Could not read template '%s': %v", t.Source, err)
		}

		name := filepath.Base(t.Source)
		newTemplate, err := template.New(name).Funcs(funcs).Parse(string(tmplBytes))
		if err != nil {
			log.Fatalf("Could not parse template '%s': %v", t.Source, err)
		}

		buf := new(bytes.Buffer)
		if err := newTemplate.Execute(buf, nil); err != nil {
			log.Fatalf("Could not render template: '%s': %v", t.Source, err)
		}

		content := buf.Bytes()

		if t.Dest == "" {
			log.Debug("No destination specified. Printing to StdOut")
			os.Stdout.Write(content)
			return nil
		}

		log.Debug("Checking whether content has changed")
		same, err := sameContent(content, t.Dest)
		if err != nil {
			return fmt.Errorf("Could not compare content for %s: %v", t.Dest, err)
		}

		if same {
			log.Debugf("Destination %s is up to date", t.Dest)
			return nil
		}

		log.Debug("Creating staging file")
		stagingFile, err := createStagingFile(content, t.Dest)
		t.Staging = stagingFile
		if err != nil {
			return err
		}

		log.Debugf("Writing destination")
		if err := copyStagingToDestination(stagingFile, t.Dest); err != nil {
			return fmt.Errorf("Could not write destination file %s: %v", t.Dest, err)
		}

		log.Info("Destination file has been updated: ", t.Dest)

		defer os.Remove(stagingFile)

	} else {
		// No source or dest - just run check/notify commands
		log.Debugf("No template - processing commands")
	}

	if t.NotifyLbl == "" {
			// Basic check/notify command, no label group
			r.runCheckNotify(t, "", "");
		} else {
			// Possible multi-container check/notify from label group
			toNotify, _ := r.getLabelGroup(t.NotifyLbl)

			for _, c := range toNotify {
				log.Debugf("Parsing: %+v", c.Name)
				parsedCheck, _ := parseCmdTemplate(c, t.CheckCmd)
				parsedNotify, _ := parseCmdTemplate(c, t.NotifyCmd)

				err := r.runCheckNotify(t, parsedCheck, parsedNotify);
				if err != nil {
					fmt.Errorf("Check notification failed for check: %v\nnotify: %v\nError: %v", parsedCheck, parsedNotify, err)
				}
			}
		}

	return nil
}

func (r *runner) runCheckNotify(t Template, parsedCheck string, parsedNotify string) error {
	var err error

	checkCmd := ""
	if parsedCheck != "" {
		checkCmd = parsedCheck
	} else {
		checkCmd = t.CheckCmd
	}

	if checkCmd != "" {
		command := strings.Replace(checkCmd, "{{staging}}", t.Staging, -1)
		if err := check(command); err != nil {
			return fmt.Errorf("Check command failed: %v", err)
		}
	}

	notifyCmd := ""
	if parsedNotify != "" {
		notifyCmd = parsedNotify
	} else {
		notifyCmd = t.NotifyCmd
	}

	if notifyCmd != "" {
		if err := notify(notifyCmd, t.NotifyOutput); err != nil {
			return fmt.Errorf("Notify command failed: %v", err)
		}
	}

	return err
}

func (r *runner) getLabelGroup(label string) ([]Container, error){
	nLabelName, nLabelValue := "", ""
	toNotify := []Container{} // may be more than just Containers in the future

	if label == "" {
		return nil, fmt.Errorf("NotifyLabelGroup failed: no label specified")
	}

	split := strings.Split(label, ":")
	nLabelName = split[0]

	// Handle labels with and without values
	if len(split) > 1 {
		nLabelValue = split[1]
		log.Debugf("Notifying label '%v' with value '%v'", nLabelName, nLabelValue)
	} else {
		log.Debugf("Notifying label '%s'", nLabelName)
	}

	// Populate `ctx` with system metadata
	ctx, err := r.createContext()
	if err != nil {
		time.Sleep(time.Second * 2)
		return nil, fmt.Errorf("Failed to create context from Rancher Metadata: %v", err)
	}

	// Search Services?
	// Search Hosts?
	// Search Containers:
	for _, c := range ctx.Containers {
		for lbl, val := range c.Labels {
			if lbl == nLabelName {
				if (nLabelValue == "") || (val == nLabelValue) {
					log.Debugf("NOTIFY: %+v :: [%+v:%+v]", c.Name, lbl, val)
					toNotify = append(toNotify, c)
				}
			}
		}
	}

	return toNotify, err;
}

func parseCmdTemplate(c Container, command string) (string, error) {
	ret := command
  reg, _ := regexp.Compile(`{{[\w\.]*}}`)
  matches := reg.FindAll( []byte(ret), -1)
	cStruct := structs.New(c)

  for _, match := range matches {
    key := strings.Trim(string(match), "{}")
		if strings.Index(key, ".") == 0{
			key = strings.Replace(key, ".", "", 1)
		}

		if strings.Contains(key, "Labels.") {
			labelParts := strings.SplitAfterN(key, ".", 2)
			label := labelParts[len(labelParts)-1]
			ret = strings.Replace(ret, string(match), c.Labels[label], -1)
		} else {
			// First check to see if key is a field in this struct
			for _, f := range cStruct.Fields(){
				if f.Name() == key{
					val, _ := cStruct.Field(key).Value().(string)
					ret = strings.Replace(ret, string(match), val, -1)
				}
			}
		}
  }

	return ret, nil
}

func copyStagingToDestination(stagingPath, destPath string) error {
	err := os.Rename(stagingPath, destPath)
	if err == nil {
		return nil
	}

	if !strings.Contains(err.Error(), "device or resource busy") {
		return err
	}

	// A 'device busy' error could mean that the files live in
	// different mounts. Try to read the staging file and write
	// it's content to the destination file.
	log.Debugf("Failed to rename staging file: %v", err)

	content, err := ioutil.ReadFile(stagingPath)
	if err != nil {
		return err
	}

	sfi, err := os.Stat(stagingPath)
	if err != nil {
		return err
	}

	if err := ioutil.WriteFile(destPath, content, sfi.Mode()); err != nil {
		return err
	}

	if os_stat, ok := sfi.Sys().(*syscall.Stat_t); ok {
		if err := os.Chown(destPath, int(os_stat.Uid), int(os_stat.Gid)); err != nil {
			return err
		}
	}

	return nil
}

func (r *runner) createContext() (*TemplateContext, error) {
	log.Debug("Fetching Metadata")

	metaServices, err := r.Client.GetServices()
	if err != nil {
		return nil, err
	}
	metaContainers, err := r.Client.GetContainers()
	if err != nil {
		return nil, err
	}
	metaHosts, err := r.Client.GetHosts()
	if err != nil {
		return nil, err
	}
	metaSelf, err := r.Client.GetSelfContainer()
	if err != nil {
		return nil, err
	}

	hosts := make([]Host, 0)
	for _, h := range metaHosts {
		host := Host{
			UUID:     h.UUID,
			Name:     h.Name,
			Address:  h.AgentIP,
			Hostname: h.Hostname,
			Labels:   LabelMap(h.Labels),
		}
		hosts = append(hosts, host)
	}

	containers := make([]Container, 0)
	for _, c := range metaContainers {
		container := Container{
			Name:    c.Name,
			Address: c.PrimaryIp,
			Stack:   c.StackName,
			Service: c.ServiceName,
			Health:  c.HealthState,
			State:   c.State,
			Labels:  LabelMap(c.Labels),
		}
		for _, h := range hosts {
			if h.UUID == c.HostUUID {
				container.Host = h
				break
			}
		}
		containers = append(containers, container)
	}

	services := make([]Service, 0)
	for _, s := range metaServices {
		service := Service{
			Name:     s.Name,
			Stack:    s.StackName,
			Kind:     s.Kind,
			Vip:      s.Vip,
			Fqdn:     s.Fqdn,
			Labels:   LabelMap(s.Labels),
			Metadata: MetadataMap(s.Metadata),
		}
		svcContainers := make([]Container, 0)
		for _, c := range containers {
			if c.Stack == s.StackName && c.Service == s.Name {
				svcContainers = append(svcContainers, c)
			}
		}
		service.Containers = svcContainers
		service.Ports = parseServicePorts(s.Ports)
		services = append(services, service)
	}

	self := Self{
		Stack:    metaSelf.StackName,
		Service:  metaSelf.ServiceName,
		HostUUID: metaSelf.HostUUID,
	}

	ctx := TemplateContext{
		Services:   services,
		Containers: containers,
		Hosts:      hosts,
		Self:       self,
	}

	return &ctx, nil
}

// converts Metadata.Service.Ports string slice to a ServicePort slice
func parseServicePorts(ports []string) []ServicePort {
	var ret []ServicePort
	for _, port := range ports {
		if parts := strings.Split(port, ":"); len(parts) == 2 {
			public := parts[0]
			if parts_ := strings.Split(parts[1], "/"); len(parts_) == 2 {
				ret = append(ret, ServicePort{
					PublicPort:   public,
					InternalPort: parts_[0],
					Protocol:     parts_[1],
				})
				continue
			}
		}
		log.Warnf("Unexpected format of service port: %s", port)
	}

	return ret
}

func check(command string) error {
	//command = strings.Replace(command, "{{staging}}", filePath, -1)
	log.Debugf("Running check command '%s'", command)
	cmd := exec.Command("/bin/sh", "-c", command)
	out, err := cmd.CombinedOutput()

	if err != nil {
		log.Printf("Check failed, skipping notify-cmd");
		logCmdOutput(command, out)
		return err
	}

	//log.Debugf("Check cmd output: %q", string(out))
	return nil
}

func notify(command string, verbose bool) error {
	log.Infof("Executing notify command '%s'", command)
	cmd := exec.Command("/bin/sh", "-c", command)
	out, err := cmd.CombinedOutput()
	if err != nil {
		logCmdOutput(command, out)
		return err
	}

	if verbose {
		logCmdOutput(command, out)
	}

	log.Debugf("Notify cmd output: %q", string(out))
	return nil
}

func logCmdOutput(command string, output []byte) {
	for _, line := range strings.Split(string(output), "\n") {
		if line != "" {
			log.Infof("[%s]: %q", command, line)
		}
	}
}

func sameContent(content []byte, filePath string) (bool, error) {
	fileMd5, err := computeFileMd5(filePath)
	if err != nil {
		return false, fmt.Errorf("Could not calculate checksum for %s: %v",
			filePath, err)
	}

	hash := md5.New()
	hash.Write(content)
	contentMd5 := fmt.Sprintf("%x", hash.Sum(nil))

	log.Debugf("Checksum content: %s, checksum file: %s",
		contentMd5, fileMd5)

	if fileMd5 == contentMd5 {
		return true, nil
	}

	return false, nil
}

func computeFileMd5(filePath string) (string, error) {
	if _, err := os.Stat(filePath); err != nil {
		return "", nil
	}

	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := md5.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}

	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func createStagingFile(content []byte, destFile string) (string, error) {
	fp, err := ioutil.TempFile(filepath.Dir(destFile), "."+filepath.Base(destFile)+"-")
	if err != nil {
		return "", fmt.Errorf("Could not create staging file for %s: %v", destFile, err)
	}

	log.Debugf("Created staging file %s", fp.Name())

	onErr := func() {
		fp.Close()
		os.Remove(fp.Name())
	}

	if _, err := fp.Write(content); err != nil {
		onErr()
		return "", fmt.Errorf("Could not write staging file for %s: %v", destFile, err)
	}

	log.Debug("Copying file permissions and owner from destination")
	if stat, err := os.Stat(destFile); err == nil {
		if err := fp.Chmod(stat.Mode()); err != nil {
			onErr()
			return "", fmt.Errorf("Failed to copy permissions from %s: %v", destFile, err)
		}
		if os_stat, ok := stat.Sys().(*syscall.Stat_t); ok {
			if err := fp.Chown(int(os_stat.Uid), int(os_stat.Gid)); err != nil {
				onErr()
				return "", fmt.Errorf("Failed to copy ownership: %v", err)
			}
		}
	}

	fp.Close()
	return fp.Name(), nil
}

