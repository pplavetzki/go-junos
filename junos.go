// Package junos provides automation for Junos (Juniper Networks) devices, as
// well as interaction with Junos Space.
package junos

import (
	"crypto/x509"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"io/ioutil"
	"regexp"
	"strings"
	"time"

	"github.com/Juniper/go-netconf/netconf"
	"golang.org/x/crypto/ssh"
)

// All of our RPC calls we use.
var (
	rpcCommand             = "<command format=\"text\">%s</command>"
	rpcCommandXML          = "<command format=\"xml\">%s</command>"
	rpcCommit              = "<commit-configuration/>"
	rpcCommitAt            = "<commit-configuration><at-time>%s</at-time></commit-configuration>"
	rpcCommitAtLog         = "<commit-configuration><at-time>%s</at-time><log>%s</log></commit-configuration>"
	rpcCommitCheck         = "<commit-configuration><check/></commit-configuration>"
	rpcCommitConfirm       = "<commit-configuration><confirmed/><confirm-timeout>%d</confirm-timeout></commit-configuration>"
	rpcCommitFull          = "<commit-configuration><full/></commit-configuration>"
	rpcFactsRE             = "<get-route-engine-information/>"
	rpcFactsChassis        = "<get-chassis-inventory/>"
	rpcConfigFileSet       = "<load-configuration action=\"set\" format=\"text\"><configuration-set>%s</configuration-set></load-configuration>"
	rpcConfigFileText      = "<load-configuration format=\"text\"><configuration-text>%s</configuration-text></load-configuration>"
	rpcConfigFileXML       = "<load-configuration format=\"xml\"><configuration>%s</configuration></load-configuration>"
	rpcConfigURLSet        = "<load-configuration action=\"set\" format=\"text\" url=\"%s\"/>"
	rpcConfigURLText       = "<load-configuration format=\"text\" url=\"%s\"/>"
	rpcConfigURLXML        = "<load-configuration format=\"xml\" url=\"%s\"/>"
	rpcConfigStringSet     = "<load-configuration action=\"set\" format=\"text\"><configuration-set>%s</configuration-set></load-configuration>"
	rpcConfigStringText    = "<load-configuration format=\"text\"><configuration-text>%s</configuration-text></load-configuration>"
	rpcConfigStringXML     = "<load-configuration format=\"xml\"><configuration>%s</configuration></load-configuration>"
	rpcGetRescue           = "<get-rescue-information><format>text</format></get-rescue-information>"
	rpcGetRollback         = "<get-rollback-information><rollback>%d</rollback><format>text</format></get-rollback-information>"
	rpcGetRollbackCompare  = "<get-rollback-information><rollback>0</rollback><compare>%d</compare><format>text</format></get-rollback-information>"
	rpcGetCandidateCompare = "<get-configuration compare=\"rollback\" rollback=\"%d\" format=\"text\"/>"
	rpcHardware            = "<get-chassis-inventory/>"
	rpcLock                = "<lock-configuration/>"
	rpcRescueConfig        = "<load-configuration rescue=\"rescue\"/>"
	rpcRescueDelete        = "<request-delete-rescue-configuration/>"
	rpcRescueSave          = "<request-save-rescue-configuration/>"
	rpcRollbackConfig      = "<load-configuration rollback=\"%d\"/>"
	rpcRoute               = "<get-route-engine-information/>"
	rpcSoftware            = "<get-software-information/>"
	rpcUnlock              = "<unlock-configuration/>"
	rpcVersion             = "<get-software-information/>"
	rpcReboot              = "<request-reboot/>"
	rpcCommitHistory       = "<get-commit-information/>"
	rpcFileList            = "<file-list><detail/><path>%s</path></file-list>"
	rpcInterfaces          = "<get-interface-information/>"
)

// Junos contains our session state.
type Junos struct {
	Session        *netconf.Session
	Hostname       string
	RoutingEngines int
	Platform       []RoutingEngine
	CommitTimeout  time.Duration
}

// AuthMethod defines how we want to authenticate to the device. If using a
// username and password to authenticate, the Credentials field must contain the username and password
//, respectively (i.e. []string{"admin", "password"}). If you are using an SSH key file for
// authentication, provide SSH username, passphrase and the path to the file for their respective fields.
// type AuthMethod struct {
// 	Credentials   []string
// 	SSHUsername   string
// 	SSHPassphrase string
// 	SSHKeyFile    string
// }

// CommitHistory holds all of the commit entries.
type CommitHistory struct {
	Entries []CommitEntry `xml:"commit-history"`
}

// CommitEntry holds information about each prevous commit.
type CommitEntry struct {
	Sequence  int    `xml:"sequence-number"`
	User      string `xml:"user"`
	Method    string `xml:"client"`
	Log       string `xml:"log"`
	Comment   string `xml:"comment"`
	Timestamp string `xml:"date-time"`
}

// RoutingEngine contains the hardware and software information for each route engine.
type RoutingEngine struct {
	Model   string
	Version string
}

type commandXML struct {
	Config string `xml:",innerxml"`
}

type commitError struct {
	Path    string `xml:"error-path"`
	Element string `xml:"error-info>bad-element"`
	Message string `xml:"error-message"`
}

type commitResults struct {
	XMLName xml.Name      `xml:"commit-results"`
	Errors  []commitError `xml:"rpc-error"`
}

type diffXML struct {
	XMLName xml.Name `xml:"rollback-information"`
	Error   string   `xml:"rpc-error>error-message"`
	Config  string   `xml:"configuration-information>configuration-output"`
}

// cdiffXML - candidate config diff XML
type cdiffXML struct {
	XMLName xml.Name `xml:"configuration-information"`
	Error   string   `xml:"rpc-error>error-message"`
	Config  string   `xml:"configuration-output"`
}

type hardwareRouteEngines struct {
	XMLName xml.Name              `xml:"multi-routing-engine-results"`
	RE      []hardwareRouteEngine `xml:"multi-routing-engine-item>chassis-inventory"`
}

type hardwareRouteEngine struct {
	XMLName     xml.Name `xml:"chassis-inventory"`
	Serial      string   `xml:"chassis>serial-number"`
	Description string   `xml:"chassis>description"`
}

type versionRouteEngines struct {
	XMLName xml.Name             `xml:"multi-routing-engine-results"`
	RE      []versionRouteEngine `xml:"multi-routing-engine-item>software-information"`
}

type versionRouteEngine struct {
	XMLName     xml.Name             `xml:"software-information"`
	Hostname    string               `xml:"host-name"`
	Platform    string               `xml:"product-model"`
	PackageInfo []versionPackageInfo `xml:"package-information"`
}

type versionPackageInfo struct {
	XMLName         xml.Name `xml:"package-information"`
	PackageName     []string `xml:"name"`
	SoftwareVersion []string `xml:"comment"`
}

// SSHConfigPassword is a convenience function that takes a username and password
// and returns a new ssh.ClientConfig setup to pass that username and password.
// Convenience means that HostKey checks are disabled so it's probably less secure
func SSHConfigPubKeyFile(user string, file string, passphrase string) (*ssh.ClientConfig, error) {
	buf, err := ioutil.ReadFile(file)
	if err != nil {
		return nil, err
	}
	block, rest := pem.Decode(buf)
	if len(rest) > 0 {
		return nil, fmt.Errorf("pem: unable to decode file %s", file)
	}

	if x509.IsEncryptedPEMBlock(block) {
		b := block.Bytes
		b, err = x509.DecryptPEMBlock(block, []byte(passphrase))
		if err != nil {
			return nil, err
		}
		buf = pem.EncodeToMemory(&pem.Block{
			Type:  block.Type,
			Bytes: b,
		})
	}

	key, err := ssh.ParsePrivateKey(buf)
	if err != nil {
		return nil, err
	}
	return &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(key),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}, nil

}

// NewSession establishes a new connection to a Junos device that we will use
// to run our commands against. NewSession also gathers software information
// about the device. Logger is optional for additonal NETCONF logging
// logger is any logger that implements the netconf.Logger interface (ex: logrus)
func NewSession(host string, config *ssh.ClientConfig) (*Junos, error) {
	rex := regexp.MustCompile(`^.*\[(.*)\]`)

	session, err := netconf.DialSSH(host, config)
	if err != nil {
		panic(fmt.Errorf("error connecting to %s - %s", host, err))
	}

	reply, err := session.Exec(netconf.RawMethod(rpcVersion))
	if err != nil {
		return nil, err
	}

	if reply.Errors != nil {
		for _, m := range reply.Errors {
			return nil, errors.New(m.Message)
		}
	}

	formatted := strings.Replace(reply.Data, "\n", "", -1)

	if strings.Contains(reply.Data, "multi-routing-engine-results") {
		var facts versionRouteEngines
		err = xml.Unmarshal([]byte(formatted), &facts)
		if err != nil {
			return nil, err
		}

		numRE := len(facts.RE)
		hostname := facts.RE[0].Hostname
		res := make([]RoutingEngine, 0, numRE)

		for i := 0; i < numRE; i++ {
			version := rex.FindStringSubmatch(facts.RE[i].PackageInfo[0].SoftwareVersion[0])
			model := strings.ToUpper(facts.RE[i].Platform)
			res = append(res, RoutingEngine{Model: model, Version: version[1]})
		}

		return &Junos{
			Session:        session,
			Hostname:       hostname,
			RoutingEngines: numRE,
			Platform:       res,
			CommitTimeout:  0,
		}, nil
	}

	var facts versionRouteEngine
	err = xml.Unmarshal([]byte(formatted), &facts)
	if err != nil {
		return nil, err
	}

	// res := make([]RoutingEngine, 0)
	var res []RoutingEngine
	hostname := facts.Hostname
	version := rex.FindStringSubmatch(facts.PackageInfo[0].SoftwareVersion[0])
	model := strings.ToUpper(facts.Platform)
	res = append(res, RoutingEngine{Model: model, Version: version[1]})

	return &Junos{
		Session:        session,
		Hostname:       hostname,
		RoutingEngines: 1,
		Platform:       res,
		CommitTimeout:  0,
	}, nil
}

// Close disconnects our session to the device.
func (j *Junos) Close() {
	j.Session.Transport.Close()
}

// Command executes any operational mode command, such as "show" or "request." If you wish to return the results
// of the command, specify the format, which must be "text" or "xml" as the second parameter (optional).
func (j *Junos) Command(cmd string, format ...string) (string, error) {
	var command string
	command = fmt.Sprintf(rpcCommand, cmd)

	if len(format) > 0 && format[0] == "xml" {
		command = fmt.Sprintf(rpcCommandXML, cmd)
	}

	reply, err := j.Session.Exec(netconf.RawMethod(command))
	if err != nil {
		return "", err
	}

	if reply.Errors != nil {
		for _, m := range reply.Errors {
			return "", errors.New(m.Message)
		}
	}

	if reply.Data == "" {
		return "", errors.New("no output available - please check the syntax of your command")
	}

	if len(format) > 0 && format[0] == "text" {
		var output commandXML
		err = xml.Unmarshal([]byte(reply.Data), &output)
		if err != nil {
			return "", err
		}

		return output.Config, nil
	}

	return reply.Data, nil
}

// CommitHistory gathers all the information about the previous 5 commits.
func (j *Junos) CommitHistory() (*CommitHistory, error) {
	var history CommitHistory
	reply, err := j.Session.Exec(netconf.RawMethod(rpcCommitHistory))
	if err != nil {
		return nil, err
	}

	if reply.Errors != nil {
		for _, m := range reply.Errors {
			return nil, errors.New(m.Message)
		}
	}

	if reply.Data == "" {
		return nil, errors.New("could not load commit history")
	}

	formatted := strings.Replace(reply.Data, "\n", "", -1)
	err = xml.Unmarshal([]byte(formatted), &history)
	if err != nil {
		return nil, err
	}

	return &history, nil
}

// Commit commits the configuration.
func (j *Junos) Commit() error {
	var errs commitResults
	reply, err := j.Session.Exec(netconf.RawMethod(rpcCommit))
	if err != nil {
		return err
	}

	if reply.Errors != nil {
		for _, m := range reply.Errors {
			return errors.New(m.Message)
		}
	}

	formatted := strings.Replace(reply.Data, "\n", "", -1)
	err = xml.Unmarshal([]byte(formatted), &errs)
	if err != nil {
		return err
	}

	if errs.Errors != nil {
		for _, m := range errs.Errors {
			return errors.New(strings.Trim(m.Message, "[\r\n]"))
		}
	}

	if j.CommitTimeout > 0 {
		time.Sleep(j.CommitTimeout * time.Second)
	}

	return nil
}

// CommitAt commits the configuration at the specified time. Time must be in 24-hour HH:mm format.
// Specifying a commit message is optional.
func (j *Junos) CommitAt(time string, message ...string) error {
	var errs commitResults
	command := fmt.Sprintf(rpcCommitAt, time)

	if len(message) > 0 {
		command = fmt.Sprintf(rpcCommitAtLog, time)
	}

	reply, err := j.Session.Exec(netconf.RawMethod(command))
	if err != nil {
		return err
	}

	if reply.Errors != nil {
		for _, m := range reply.Errors {
			return errors.New(m.Message)
		}
	}

	formatted := strings.Replace(reply.Data, "\n", "", -1)
	err = xml.Unmarshal([]byte(formatted), &errs)
	if err != nil {
		return err
	}

	if errs.Errors != nil {
		for _, m := range errs.Errors {
			return errors.New(strings.Trim(m.Message, "[\r\n]"))
		}
	}

	return nil
}

// CommitCheck checks the configuration for syntax errors, but does not commit any changes.
func (j *Junos) CommitCheck() error {
	var errs commitResults
	reply, err := j.Session.Exec(netconf.RawMethod(rpcCommitCheck))
	if err != nil {
		return err
	}

	if reply.Errors != nil {
		for _, m := range reply.Errors {
			return errors.New(m.Message)
		}
	}

	formatted := strings.Replace(reply.Data, "\n", "", -1)
	err = xml.Unmarshal([]byte(formatted), &errs)
	if err != nil {
		return err
	}

	if errs.Errors != nil {
		for _, m := range errs.Errors {
			return errors.New(strings.Trim(m.Message, "[\r\n]"))
		}
	}

	return nil
}

// CommitConfirm rolls back the configuration after the delayed minutes.
func (j *Junos) CommitConfirm(delay int) error {
	var errs commitResults
	command := fmt.Sprintf(rpcCommitConfirm, delay)
	reply, err := j.Session.Exec(netconf.RawMethod(command))
	if err != nil {
		return err
	}

	if reply.Errors != nil {
		for _, m := range reply.Errors {
			return errors.New(m.Message)
		}
	}

	formatted := strings.Replace(reply.Data, "\n", "", -1)
	err = xml.Unmarshal([]byte(formatted), &errs)
	if err != nil {
		return err
	}

	if errs.Errors != nil {
		for _, m := range errs.Errors {
			message := fmt.Sprintf("[%s]\n    %s\nError: %s", strings.Trim(m.Path, "[\r\n]"), strings.Trim(m.Element, "[\r\n]"), strings.Trim(m.Message, "[\r\n]"))
			return errors.New(message)
		}
	}

	return nil
}

// Diff compares candidate config to current (rollback 0) or previous rollback
// this is equivalent to 'show | compare' or 'show | compare rollback X' when
// in configuration mode
// RPC: <get-configuration compare="rollback" rollback="[0-49]" format="text"/>
// https://goo.gl/wFRMX9 (juniper.net)
func (j *Junos) Diff(rollback int) (string, error) {
	var cd cdiffXML
	command := fmt.Sprintf(rpcGetCandidateCompare, rollback)
	reply, err := j.Session.Exec(netconf.RawMethod(command))
	if err != nil {
		return "", err
	}

	if reply.Errors != nil {
		for _, m := range reply.Errors {
			return "", errors.New(m.Message)
		}
	}

	// formatted := strings.Replace(reply.Data, "\n", "", -1)
	err = xml.Unmarshal([]byte(reply.Data), &cd)
	if err != nil {
		return "", err
	}

	if cd.Error != "" {
		errMessage := strings.Trim(cd.Error, "\r\n")
		return "", errors.New(errMessage)
	}

	return cd.Config, nil
}

// GetConfig returns the configuration starting at the given section. If you do not specify anything
// for section, then the entire configuration will be returned. Format must be "text" or "xml." You
// can do sub-sections by separating the section path with a ">" symbol, i.e. "system>login" or "protocols>ospf>area."
// The default option is to return the XML.
func (j *Junos) GetConfig(format string, section ...string) (string, error) {
	command := fmt.Sprintf("<get-configuration format=\"%s\"><configuration>", format)

	if len(section) > 0 {
		secs := strings.Split(section[0], ">")
		nSecs := len(secs) - 1

		if nSecs >= 0 {
			for i := 0; i < nSecs; i++ {
				command += fmt.Sprintf("<%s>", secs[i])
			}
			command += fmt.Sprintf("<%s/>", secs[nSecs])

			for j := nSecs - 1; j >= 0; j-- {
				command += fmt.Sprintf("</%s>", secs[j])
			}
			command += fmt.Sprint("</configuration></get-configuration>")
		}
	}

	if len(section) <= 0 {
		command += "</configuration></get-configuration>"
	}

	reply, err := j.Session.Exec(netconf.RawMethod(command))
	if err != nil {
		return "", err
	}

	if len(reply.Data) < 50 {
		return "", errors.New("the section you provided is not configured on the device")
	}

	if reply.Errors != nil {
		for _, m := range reply.Errors {
			return "", errors.New(m.Message)
		}
	}

	switch format {
	case "text":
		var output commandXML
		// formatted := strings.Replace(reply.Data, "\n", "", -1)
		err = xml.Unmarshal([]byte(reply.Data), &output)
		if err != nil {
			return "", err
		}

		if len(output.Config) <= 1 {
			return "", errors.New("the section you provided is not configured on the device")
		}

		return output.Config, nil
	case "xml":
		return reply.Data, nil
	}

	return reply.Data, nil
}

// Config loads a given configuration file from your local machine,
// a remote (FTP or HTTP server) location, or via configuration statements
// from variables (type string or []string) within your script. Format must be
// "set", "text" or "xml".
func (j *Junos) Config(path interface{}, format string, commit bool) error {
	var command string
	switch format {
	case "set":
		switch path.(type) {
		case string:
			if strings.Contains(path.(string), "tp://") {
				command = fmt.Sprintf(rpcConfigURLSet, path.(string))
			}

			if _, err := ioutil.ReadFile(path.(string)); err != nil {
				command = fmt.Sprintf(rpcConfigStringSet, path.(string))
			} else {
				data, err := ioutil.ReadFile(path.(string))
				if err != nil {
					return err
				}

				command = fmt.Sprintf(rpcConfigFileSet, string(data))
			}
		case []string:
			command = fmt.Sprintf(rpcConfigStringSet, strings.Join(path.([]string), "\n"))
		}
	case "text":
		switch path.(type) {
		case string:
			if strings.Contains(path.(string), "tp://") {
				command = fmt.Sprintf(rpcConfigURLText, path.(string))
			}

			if _, err := ioutil.ReadFile(path.(string)); err != nil {
				command = fmt.Sprintf(rpcConfigStringText, path.(string))
			} else {
				data, err := ioutil.ReadFile(path.(string))
				if err != nil {
					return err
				}

				command = fmt.Sprintf(rpcConfigFileText, string(data))
			}
		case []string:
			command = fmt.Sprintf(rpcConfigStringText, strings.Join(path.([]string), "\n"))
		}
	case "xml":
		switch path.(type) {
		case string:
			if strings.Contains(path.(string), "tp://") {
				command = fmt.Sprintf(rpcConfigURLXML, path.(string))
			}

			if _, err := ioutil.ReadFile(path.(string)); err != nil {
				command = fmt.Sprintf(rpcConfigStringXML, path.(string))
			} else {
				data, err := ioutil.ReadFile(path.(string))
				if err != nil {
					return err
				}

				command = fmt.Sprintf(rpcConfigFileXML, string(data))
			}
		case []string:
			command = fmt.Sprintf(rpcConfigStringXML, strings.Join(path.([]string), "\n"))
		}
	}

	reply, err := j.Session.Exec(netconf.RawMethod(command))
	if err != nil {
		return err
	}

	if commit {
		err = j.Commit()
		if err != nil {
			return err
		}
	}

	if reply.Errors != nil {
		for _, m := range reply.Errors {
			return errors.New(m.Message)
		}
	}

	return nil
}

// Lock locks the candidate configuration.
func (j *Junos) Lock() error {
	reply, err := j.Session.Exec(netconf.RawMethod(rpcLock))
	if err != nil {
		return err
	}

	if reply.Errors != nil {
		for _, m := range reply.Errors {
			return errors.New(m.Message)
		}
	}

	if j.CommitTimeout > 0 {
		time.Sleep(j.CommitTimeout * time.Second)
	}

	return nil
}

// Rescue will create or delete the rescue configuration given "save" or "delete" for the action.
func (j *Junos) Rescue(action string) error {
	if action != "save" || action != "delete" {
		return errors.New("you must specify save or delete for a rescue config action")
	}

	command := fmt.Sprintf(rpcRescueSave)

	if action == "delete" {
		command = fmt.Sprintf(rpcRescueDelete)
	}

	reply, err := j.Session.Exec(netconf.RawMethod(command))
	if err != nil {
		return err
	}

	if reply.Errors != nil {
		for _, m := range reply.Errors {
			return errors.New(m.Message)
		}
	}

	return nil
}

// Rollback loads and commits the configuration of a given rollback number or rescue state, by specifying "rescue."
func (j *Junos) Rollback(option interface{}) error {
	var command = fmt.Sprintf(rpcRollbackConfig, option)

	if option == "rescue" {
		command = fmt.Sprintf(rpcRescueConfig)
	}

	reply, err := j.Session.Exec(netconf.RawMethod(command))
	if err != nil {
		return err
	}

	err = j.Commit()
	if err != nil {
		return err
	}

	if reply.Errors != nil {
		for _, m := range reply.Errors {
			return errors.New(m.Message)
		}
	}

	return nil
}

// Unlock unlocks the candidate configuration.
func (j *Junos) Unlock() error {
	reply, err := j.Session.Exec(netconf.RawMethod(rpcUnlock))
	if err != nil {
		return err
	}

	if reply.Errors != nil {
		for _, m := range reply.Errors {
			return errors.New(m.Message)
		}
	}

	if j.CommitTimeout > 0 {
		time.Sleep(j.CommitTimeout * time.Second)
	}

	return nil
}

// Reboot will reboot the device.
func (j *Junos) Reboot() error {
	reply, err := j.Session.Exec(netconf.RawMethod(rpcReboot))
	if err != nil {
		return err
	}

	if reply.Errors != nil {
		for _, m := range reply.Errors {
			return errors.New(m.Message)
		}
	}

	return nil
}

// CommitFull does a full commit on the configuration, which requires all daemons to
// check and evaluate the new configuration. Useful for when you get an error with
// a commit or when you've changed the configuration significantly.
func (j *Junos) CommitFull() error {
	reply, err := j.Session.Exec(netconf.RawMethod(rpcCommitFull))
	if err != nil {
		return err
	}

	if reply.Errors != nil {
		for _, m := range reply.Errors {
			return errors.New(m.Message)
		}
	}

	return nil
}

// SetCommitTimeout will add the given delay time (in seconds) to the following commit functions: Lock(),
// Commit() and Unlock(). When configuring multiple devices, or having a large configuration to push, this can
// greatly reduce errors (especially if you're dealing with latency).
func (j *Junos) SetCommitTimeout(delay int) {
	d := time.Duration(delay)

	j.CommitTimeout = d
}
