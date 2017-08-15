package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/packethost/packngo"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/terminal"
)

const (
	packetDefaultZone    = "ams1"
	packetDefaultMachine = "baremetal_0"
	packetBaseURL        = "PACKET_BASE_URL"
	packetZoneVar        = "PACKET_ZONE"
	packetMachineVar     = "PACKET_MACHINE"
	packetAPIKeyVar      = "PACKET_API_KEY"
	packetProjectIDVar   = "PACKET_PROJECT_ID"
	packetHostnameVar    = "PACKET_HOSTNAME"
	packetNameVar        = "PACKET_NAME"
)

var (
	packetDefaultHostname = "linuxkit"
)

func init() {
	// Prefix host name with username
	if u, err := user.Current(); err == nil {
		packetDefaultHostname = u.Username + "-" + packetDefaultHostname
	}
}

// Process the run arguments and execute run
func runPacket(args []string) {
	flags := flag.NewFlagSet("packet", flag.ExitOnError)
	invoked := filepath.Base(os.Args[0])
	flags.Usage = func() {
		fmt.Printf("USAGE: %s run packet [options] [name]\n\n", invoked)
		fmt.Printf("Options:\n\n")
		flags.PrintDefaults()
	}
	baseURLFlag := flags.String("base-url", "", "Base URL that the kernel and initrd are served from (or "+packetBaseURL+")")
	zoneFlag := flags.String("zone", packetDefaultZone, "Packet Zone (or "+packetZoneVar+")")
	machineFlag := flags.String("machine", packetDefaultMachine, "Packet Machine Type (or "+packetMachineVar+")")
	apiKeyFlag := flags.String("api-key", "", "Packet API key (or "+packetAPIKeyVar+")")
	projectFlag := flags.String("project-id", "", "Packet Project ID (or "+packetProjectIDVar+")")
	hostNameFlag := flags.String("hostname", packetDefaultHostname, "Hostname of new instance (or "+packetHostnameVar+")")
	nameFlag := flags.String("img-name", "", "Overrides the prefix used to identify the files. Defaults to [name] (or "+packetNameVar+")")
	alwaysPXE := flags.Bool("always-pxe", true, "Reboot from PXE every time.")
	serveFlag := flags.String("serve", "", "Serve local files via the http port specified, e.g. ':8080'.")
	consoleFlag := flags.Bool("console", true, "Provide interactive access on the console.")
	keepFlag := flags.Bool("keep", false, "Keep the machine after exiting/poweroff.")
	if err := flags.Parse(args); err != nil {
		log.Fatal("Unable to parse args")
	}

	remArgs := flags.Args()
	prefix := "packet"
	if len(remArgs) > 0 {
		prefix = remArgs[0]
	}

	url := getStringValue(packetBaseURL, *baseURLFlag, "")
	if url == "" {
		log.Fatal("Need to specify a value for --base-url where the images are hosted. This URL should contain <url>/%s-kernel and <url>/%s-initrd.img")
	}
	facility := getStringValue(packetZoneVar, *zoneFlag, "")
	plan := getStringValue(packetMachineVar, *machineFlag, defaultMachine)
	apiKey := getStringValue(packetAPIKeyVar, *apiKeyFlag, "")
	if apiKey == "" {
		log.Fatal("Must specify a Packet.net API key with --api-key")
	}
	projectID := getStringValue(packetProjectIDVar, *projectFlag, "")
	if projectID == "" {
		log.Fatal("Must specify a Packet.net Project ID with --project-id")
	}
	hostname := getStringValue(packetHostnameVar, *hostNameFlag, "")
	name := getStringValue(packetNameVar, *nameFlag, prefix)
	osType := "custom_ipxe"
	billing := "hourly"

	if !*keepFlag && !*consoleFlag {
		log.Fatalf("Combination of keep=%t and console=%t makes little sense", *keepFlag, *consoleFlag)
	}

	// Read kernel command line
	var cmdline string
	if c, err := ioutil.ReadFile(prefix + "-cmdline"); err != nil {
		log.Fatalf("Cannot open cmdline file: %v", err)
	} else {
		cmdline = string(c)
	}

	// Serve files with a local http server
	var httpServer *http.Server
	if *serveFlag != "" {
		fs := serveFiles{[]string{fmt.Sprintf("%s-kernel", name), fmt.Sprintf("%s-initrd.img", name)}}
		httpServer = &http.Server{Addr: ":8080", Handler: http.FileServer(fs)}
		go func() {
			log.Infof("Listening on http://%s\n", *serveFlag)
			if err := httpServer.ListenAndServe(); err != nil {
				log.Infof("http server exited with: %v", err)
			}
		}()
	}

	// Build the iPXE script
	// Note, we *append* the <prefix>-cmdline. iXPE booting will
	// need the first set of "kernel-params" and we don't want to
	// require these to be added to every YAML file.
	userData := "#!ipxe\n\n"
	userData += "dhcp\n"
	userData += fmt.Sprintf("set base-url %s\n", url)
	userData += fmt.Sprintf("set kernel-params ip=dhcp nomodeset ro serial console=ttyS1,115200 %s\n", cmdline)
	userData += fmt.Sprintf("kernel ${base-url}/%s-kernel ${kernel-params}\n", name)
	userData += fmt.Sprintf("initrd ${base-url}/%s-initrd.img\n", name)
	userData += "boot"
	log.Debugf("Using userData of:\n%s\n", userData)

	// Make sure the URL works
	initrdURL := fmt.Sprintf("%s/%s-initrd.img", url, name)
	kernelURL := fmt.Sprintf("%s/%s-kernel", url, name)
	validateHTTPURL(kernelURL)
	validateHTTPURL(initrdURL)

	client := packngo.NewClient("", apiKey, nil)
	tags := []string{}
	req := packngo.DeviceCreateRequest{
		HostName:     hostname,
		Plan:         plan,
		Facility:     facility,
		OS:           osType,
		BillingCycle: billing,
		ProjectID:    projectID,
		UserData:     userData,
		Tags:         tags,
		AlwaysPXE:    *alwaysPXE,
	}
	dev, _, err := client.Devices.Create(&req)
	if err != nil {
		log.Fatal(err)
	}
	b, err := json.MarshalIndent(dev, "", "    ")
	if err != nil {
		log.Fatal(err)
	}
	// log response json if in verbose mode
	log.Debugf("%s\n", string(b))

	sshHost := "sos." + dev.Facility.Code + ".packet.net"
	if *consoleFlag {
		// Connect to the serial console
		if err := sshSOS(dev.ID, sshHost); err != nil {
			log.Fatal(err)
		}
	} else {
		log.Printf("Machine booting")
		log.Printf("Access the console with: ssh %s@%s", dev.ID, sshHost)

		// if the serve option is present, wait till 'ctrl-c' is hit.
		// Otherwise we wouldn't serve the files
		if *serveFlag != "" {
			stop := make(chan os.Signal, 1)
			signal.Notify(stop, os.Interrupt)
			log.Printf("Hit ctrl-c to stop http server")
			<-stop
		}
	}

	// Stop the http server before exiting
	if *serveFlag != "" {
		log.Printf("Shutting down http server...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpServer.Shutdown(ctx)
	}

	if !*keepFlag {
		if _, err := client.Devices.Delete(dev.ID); err != nil {
			log.Fatalf("Unable to delete device: %v", err)
		}
	}

}

// validateHTTPURL does a sanity check that a URL returns a 200 or 300 response
func validateHTTPURL(url string) {
	log.Printf("Validating URL: %s", url)
	resp, err := http.Head(url)
	if err != nil {
		log.Fatal(err)
	}
	if resp.StatusCode >= 400 {
		log.Fatal("Got a non 200- or 300- HTTP response code: %s", resp)
	}
	log.Printf("OK: %d response code", resp.StatusCode)
}

func sshSOS(user, host string) error {
	log.Printf("console: ssh %s@%s", user, host)

	hostKey, err := sshHostKey(host)
	if err != nil {
		return fmt.Errorf("Host key not found. Maybe need to add it? %v", err)
	}

	sshConfig := &ssh.ClientConfig{
		User:            user,
		HostKeyCallback: ssh.FixedHostKey(hostKey),
		Auth: []ssh.AuthMethod{
			sshAgent(),
		},
	}

	c, err := ssh.Dial("tcp", host+":22", sshConfig)
	if err != nil {
		return fmt.Errorf("Failed to dial: %s", err)
	}

	s, err := c.NewSession()
	if err != nil {
		return fmt.Errorf("Failed to create session: %v", err)
	}
	defer s.Close()

	s.Stdout = os.Stdout
	s.Stderr = os.Stderr
	s.Stdin = os.Stdin

	modes := ssh.TerminalModes{
		ssh.ECHO:  0,
		ssh.IGNCR: 1,
	}

	width, height, err := terminal.GetSize(0)
	if err != nil {
		log.Warningf("Error getting terminal size. Ignored. %v", err)
		width = 80
		height = 40
	}
	if err := s.RequestPty("vt100", width, height, modes); err != nil {
		return fmt.Errorf("Request for PTY failed: %v", err)
	}
	oldState, err := terminal.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return err
	}
	defer terminal.Restore(0, oldState)

	// Start remote shell
	if err := s.Shell(); err != nil {
		return fmt.Errorf("Failed to start shell: %v", err)
	}

	s.Wait()
	return nil
}

// Get a ssh-agent AuthMethod
func sshAgent() ssh.AuthMethod {
	sshAgent, err := net.Dial("unix", os.Getenv("SSH_AUTH_SOCK"))
	if err != nil {
		log.Fatalf("Failed to dial ssh-agent: %v", err)
	}
	return ssh.PublicKeysCallback(agent.NewClient(sshAgent).Signers)
}

// This function returns the host key for a given host (the SOS server).
// If it can't be found, it errors
func sshHostKey(host string) (ssh.PublicKey, error) {
	f, err := os.Open(filepath.Join(os.Getenv("HOME"), ".ssh", "known_hosts"))
	if err != nil {
		return nil, fmt.Errorf("Can't open know_hosts file: %v", err)
	}
	defer f.Close()

	s := bufio.NewScanner(f)

	var hostKey ssh.PublicKey
	for s.Scan() {
		fields := strings.Split(s.Text(), " ")
		if len(fields) != 3 {
			continue
		}
		if strings.Contains(fields[0], host) {
			var err error
			hostKey, _, _, _, err = ssh.ParseAuthorizedKey(s.Bytes())
			if err != nil {
				return nil, fmt.Errorf("Error parsing %q: %v", fields[2], err)
			}
			break
		}
	}

	if hostKey == nil {
		return nil, fmt.Errorf("No hostkey for %s", host)
	}
	return hostKey, nil
}

// This implements a http.FileSystem which only responds to specific files.
type serveFiles struct {
	files []string
}

// Open implements the Open method for the serveFiles FileSystem
// implementation.
// It converts both the name from the URL and the files provided in
// the serveFiles structure into cleaned, absolute filesystem path and
// only returns the file if the requested name matches one of the
// files in the list.
func (fs serveFiles) Open(name string) (http.File, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	name = filepath.Join(cwd, filepath.FromSlash(path.Clean("/"+name)))
	for _, fn := range fs.files {
		fn = filepath.Join(cwd, filepath.FromSlash(path.Clean("/"+fn)))
		if name == fn {
			f, err := os.Open(fn)
			if err != nil {
				return nil, err
			}
			log.Infof("Serving: %s", fn)
			return f, nil
		}
	}
	return nil, fmt.Errorf("File %s not found", name)
}
