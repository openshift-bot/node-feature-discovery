/*
Copyright 2019-2021 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package nfdworker

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"

	pb "openshift/node-feature-discovery/pkg/labeler"
	"openshift/node-feature-discovery/pkg/utils"
	"openshift/node-feature-discovery/pkg/version"
	"openshift/node-feature-discovery/source"
	"openshift/node-feature-discovery/source/cpu"
	"openshift/node-feature-discovery/source/custom"
	"openshift/node-feature-discovery/source/fake"
	"openshift/node-feature-discovery/source/iommu"
	"openshift/node-feature-discovery/source/kernel"
	"openshift/node-feature-discovery/source/local"
	"openshift/node-feature-discovery/source/memory"
	"openshift/node-feature-discovery/source/network"
	"openshift/node-feature-discovery/source/panic_fake"
	"openshift/node-feature-discovery/source/pci"
	"openshift/node-feature-discovery/source/storage"
	"openshift/node-feature-discovery/source/system"
	"openshift/node-feature-discovery/source/usb"
)

var (
	stdoutLogger = log.New(os.Stdout, "", log.LstdFlags)
	stderrLogger = log.New(os.Stderr, "", log.LstdFlags)
	nodeName     = os.Getenv("NODE_NAME")
)

// Global config
type NFDConfig struct {
	Core    coreConfig
	Sources sourcesConfig
}

type coreConfig struct {
	LabelWhiteList utils.RegexpVal
	NoPublish      bool
	Sources        []string
	SleepInterval  duration
}

type sourcesConfig map[string]source.Config

// Labels are a Kubernetes representation of discovered features.
type Labels map[string]string

// Command line arguments
type Args struct {
	CaFile             string
	CertFile           string
	KeyFile            string
	ConfigFile         string
	Options            string
	Oneshot            bool
	Server             string
	ServerNameOverride string

	Overrides ConfigOverrideArgs
}

// ConfigOverrideArgs are args that override config file options
type ConfigOverrideArgs struct {
	NoPublish *bool

	// Deprecated
	LabelWhiteList *utils.RegexpVal
	SleepInterval  *time.Duration
	Sources        *utils.StringSliceVal
}

type NfdWorker interface {
	Run() error
	Stop()
}

type nfdWorker struct {
	args           Args
	clientConn     *grpc.ClientConn
	client         pb.LabelerClient
	configFilePath string
	config         *NFDConfig
	realSources    []source.FeatureSource
	stop           chan struct{} // channel for signaling stop
	testSources    []source.FeatureSource
	enabledSources []source.FeatureSource
}

type duration struct {
	time.Duration
}

// Create new NfdWorker instance.
func NewNfdWorker(args *Args) (NfdWorker, error) {
	nfd := &nfdWorker{
		args:   *args,
		config: &NFDConfig{},
		realSources: []source.FeatureSource{
			&cpu.Source{},
			&iommu.Source{},
			&kernel.Source{},
			&memory.Source{},
			&network.Source{},
			&pci.Source{},
			&storage.Source{},
			&system.Source{},
			&usb.Source{},
			&custom.Source{},
			// local needs to be the last source so that it is able to override
			// labels from other sources
			&local.Source{},
		},
		testSources: []source.FeatureSource{
			&fake.Source{},
			&panicfake.Source{},
		},
		stop: make(chan struct{}, 1),
	}

	if args.ConfigFile != "" {
		nfd.configFilePath = filepath.Clean(args.ConfigFile)
	}

	// Check TLS related args
	if args.CertFile != "" || args.KeyFile != "" || args.CaFile != "" {
		if args.CertFile == "" {
			return nfd, fmt.Errorf("--cert-file needs to be specified alongside --key-file and --ca-file")
		}
		if args.KeyFile == "" {
			return nfd, fmt.Errorf("--key-file needs to be specified alongside --cert-file and --ca-file")
		}
		if args.CaFile == "" {
			return nfd, fmt.Errorf("--ca-file needs to be specified alongside --cert-file and --key-file")
		}
	}

	return nfd, nil
}

func addConfigWatch(path string) (*fsnotify.Watcher, map[string]struct{}, error) {
	paths := make(map[string]struct{})

	// Create watcher
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return w, paths, fmt.Errorf("failed to create fsnotify watcher: %v", err)
	}

	// Add watches for all directory components so that we catch e.g. renames
	// upper in the tree
	added := false
	for p := path; ; p = filepath.Dir(p) {

		if err := w.Add(p); err != nil {
			stdoutLogger.Printf("failed to add fsnotify watch for %q: %v", p, err)
		} else {
			stdoutLogger.Printf("added fsnotify watch %q", p)
			added = true
		}

		paths[p] = struct{}{}
		if filepath.Dir(p) == p {
			break
		}
	}

	if !added {
		// Want to be sure that we watch something
		return w, paths, fmt.Errorf("failed to add any watch")
	}
	return w, paths, nil
}

func newDefaultConfig() *NFDConfig {
	return &NFDConfig{
		Core: coreConfig{
			LabelWhiteList: utils.RegexpVal{Regexp: *regexp.MustCompile("")},
			SleepInterval:  duration{60 * time.Second},
			Sources:        []string{"all"},
		},
	}
}

// Run NfdWorker client. Returns if a fatal error is encountered, or, after
// one request if OneShot is set to 'true' in the worker args.
func (w *nfdWorker) Run() error {
	stdoutLogger.Printf("Node Feature Discovery Worker %s", version.Get())
	stdoutLogger.Printf("NodeName: '%s'", nodeName)

	// Create watcher for config file and read initial configuration
	configWatch, paths, err := addConfigWatch(w.configFilePath)
	if err != nil {
		return err
	}
	if err := w.configure(w.configFilePath, w.args.Options); err != nil {
		return err
	}

	// Connect to NFD master
	err = w.connect()
	if err != nil {
		return fmt.Errorf("failed to connect: %v", err)
	}
	defer w.disconnect()

	labelTrigger := time.After(0)
	var configTrigger <-chan time.Time
	for {
		select {
		case <-labelTrigger:
			// Get the set of feature labels.
			labels := createFeatureLabels(w.enabledSources, w.config.Core.LabelWhiteList.Regexp)

			// Update the node with the feature labels.
			if w.client != nil {
				err := advertiseFeatureLabels(w.client, labels)
				if err != nil {
					return fmt.Errorf("failed to advertise labels: %s", err.Error())
				}
			}

			if w.args.Oneshot {
				return nil
			}

			if w.config.Core.SleepInterval.Duration > 0 {
				labelTrigger = time.After(w.config.Core.SleepInterval.Duration)
			}

		case e := <-configWatch.Events:
			name := filepath.Clean(e.Name)

			// If any of our paths (directories or the file itself) change
			if _, ok := paths[name]; ok {
				stdoutLogger.Printf("fsnotify event in %q detected, reconfiguring fsnotify and reloading configuration", name)

				// Blindly remove existing watch and add a new one
				if err := configWatch.Close(); err != nil {
					stderrLogger.Printf("WARNING: failed to close fsnotify watcher: %v", err)
				}
				configWatch, paths, err = addConfigWatch(w.configFilePath)
				if err != nil {
					return err
				}

				// Rate limiter. In certain filesystem operations we get
				// numerous events in quick succession and we only want one
				// config re-load
				configTrigger = time.After(time.Second)
			}

		case e := <-configWatch.Errors:
			stderrLogger.Printf("ERROR: config file watcher error: %v", e)

		case <-configTrigger:
			if err := w.configure(w.configFilePath, w.args.Options); err != nil {
				return err
			}
			// Manage connection to master
			if w.config.Core.NoPublish {
				w.disconnect()
			} else if w.clientConn == nil {
				if err := w.connect(); err != nil {
					return err
				}
			}
			// Always re-label after a re-config event. This way the new config
			// comes into effect even if the sleep interval is long (or infinite)
			labelTrigger = time.After(0)

		case <-w.stop:
			stdoutLogger.Printf("shutting down nfd-worker")
			configWatch.Close()
			return nil
		}
	}
}

// Stop NfdWorker
func (w *nfdWorker) Stop() {
	select {
	case w.stop <- struct{}{}:
	default:
	}
}

// connect creates a client connection to the NFD master
func (w *nfdWorker) connect() error {
	// Return a dummy connection in case of dry-run
	if w.config.Core.NoPublish {
		return nil
	}

	// Check that if a connection already exists
	if w.clientConn != nil {
		return fmt.Errorf("client connection already exists")
	}

	// Dial and create a client
	dialCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dialOpts := []grpc.DialOption{grpc.WithBlock()}
	if w.args.CaFile != "" || w.args.CertFile != "" || w.args.KeyFile != "" {
		// Load client cert for client authentication
		cert, err := tls.LoadX509KeyPair(w.args.CertFile, w.args.KeyFile)
		if err != nil {
			return fmt.Errorf("failed to load client certificate: %v", err)
		}
		// Load CA cert for server cert verification
		caCert, err := ioutil.ReadFile(w.args.CaFile)
		if err != nil {
			return fmt.Errorf("failed to read root certificate file: %v", err)
		}
		caPool := x509.NewCertPool()
		if ok := caPool.AppendCertsFromPEM(caCert); !ok {
			return fmt.Errorf("failed to add certificate from '%s'", w.args.CaFile)
		}
		// Create TLS config
		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      caPool,
			ServerName:   w.args.ServerNameOverride,
		}
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	} else {
		dialOpts = append(dialOpts, grpc.WithInsecure())
	}
	conn, err := grpc.DialContext(dialCtx, w.args.Server, dialOpts...)
	if err != nil {
		return err
	}
	w.clientConn = conn
	w.client = pb.NewLabelerClient(conn)

	return nil
}

// disconnect closes the connection to NFD master
func (w *nfdWorker) disconnect() {
	if w.clientConn != nil {
		w.clientConn.Close()
	}
	w.clientConn = nil
	w.client = nil
}

func (c *coreConfig) sanitize() {
	if c.SleepInterval.Duration > 0 && c.SleepInterval.Duration < time.Second {
		stderrLogger.Printf("WARNING: too short sleep-intervall specified (%s), forcing to 1s",
			c.SleepInterval.Duration.String())
		c.SleepInterval = duration{time.Second}
	}
}

func (w *nfdWorker) configureCore(c coreConfig) {
	// Determine enabled feature sourcds
	sourceList := map[string]struct{}{}
	all := false
	for _, s := range c.Sources {
		if s == "all" {
			all = true
			continue
		}
		sourceList[strings.TrimSpace(s)] = struct{}{}
	}

	w.enabledSources = []source.FeatureSource{}
	for _, s := range w.realSources {
		if _, enabled := sourceList[s.Name()]; all || enabled {
			w.enabledSources = append(w.enabledSources, s)
			delete(sourceList, s.Name())
		}
	}
	for _, s := range w.testSources {
		if _, enabled := sourceList[s.Name()]; enabled {
			w.enabledSources = append(w.enabledSources, s)
			delete(sourceList, s.Name())
		}
	}
	if len(sourceList) > 0 {
		names := make([]string, 0, len(sourceList))
		for n := range sourceList {
			names = append(names, n)
		}
		stderrLogger.Printf("WARNING: skipping unknown source(s) %q specified in core.sources (or --sources)", strings.Join(names, ", "))
	}
}

// Parse configuration options
func (w *nfdWorker) configure(filepath string, overrides string) error {
	// Create a new default config
	c := newDefaultConfig()
	allSources := append(w.realSources, w.testSources...)
	c.Sources = make(map[string]source.Config, len(allSources))
	for _, s := range allSources {
		c.Sources[s.Name()] = s.NewConfig()
	}

	// Try to read and parse config file
	if filepath != "" {
		data, err := ioutil.ReadFile(filepath)
		if err != nil {
			if os.IsNotExist(err) {
				stderrLogger.Printf("config file %q not found, using defaults", filepath)
			} else {
				return fmt.Errorf("error reading config file: %s", err)
			}
		} else {
			err = yaml.Unmarshal(data, c)
			if err != nil {
				return fmt.Errorf("Failed to parse config file: %s", err)
			}
			stdoutLogger.Printf("Configuration successfully loaded from %q", filepath)
		}
	}

	// Parse config overrides
	if err := yaml.Unmarshal([]byte(overrides), c); err != nil {
		return fmt.Errorf("Failed to parse --options: %s", err)
	}

	if w.args.Overrides.LabelWhiteList != nil {
		c.Core.LabelWhiteList = *w.args.Overrides.LabelWhiteList
	}
	if w.args.Overrides.NoPublish != nil {
		c.Core.NoPublish = *w.args.Overrides.NoPublish
	}
	if w.args.Overrides.SleepInterval != nil {
		c.Core.SleepInterval = duration{*w.args.Overrides.SleepInterval}
	}
	if w.args.Overrides.Sources != nil {
		c.Core.Sources = *w.args.Overrides.Sources
	}

	c.Core.sanitize()

	w.config = c

	w.configureCore(c.Core)

	// (Re-)configure all "real" sources, test sources are not configurable
	for _, s := range allSources {
		s.SetConfig(c.Sources[s.Name()])
	}

	return nil
}

// createFeatureLabels returns the set of feature labels from the enabled
// sources and the whitelist argument.
func createFeatureLabels(sources []source.FeatureSource, labelWhiteList regexp.Regexp) (labels Labels) {
	labels = Labels{}

	// Do feature discovery from all configured sources.
	for _, source := range sources {
		labelsFromSource, err := getFeatureLabels(source, labelWhiteList)
		if err != nil {
			stderrLogger.Printf("discovery failed for source [%s]: %s", source.Name(), err.Error())
			stderrLogger.Printf("continuing ...")
			continue
		}

		for name, value := range labelsFromSource {
			// Log discovered feature.
			stdoutLogger.Printf("%s = %s", name, value)
			labels[name] = value
		}
	}
	return labels
}

// getFeatureLabels returns node labels for features discovered by the
// supplied source.
func getFeatureLabels(source source.FeatureSource, labelWhiteList regexp.Regexp) (labels Labels, err error) {
	defer func() {
		if r := recover(); r != nil {
			stderrLogger.Printf("panic occurred during discovery of source [%s]: %v", source.Name(), r)
			err = fmt.Errorf("%v", r)
		}
	}()

	labels = Labels{}
	features, err := source.Discover()
	if err != nil {
		return nil, err
	}

	// Prefix for labels in the default namespace
	prefix := source.Name() + "-"
	switch source.(type) {
	case *local.Source:
		// Do not prefix labels from the hooks
		prefix = ""
	}

	for k, v := range features {
		// Split label name into namespace and name compoents. Use dummy 'ns'
		// default namespace because there is no function to validate just
		// the name part
		split := strings.SplitN(k, "/", 2)

		label := prefix + split[0]
		nameForValidation := "ns/" + label
		nameForWhiteListing := label

		if len(split) == 2 {
			label = k
			nameForValidation = label
			nameForWhiteListing = split[1]
		}

		// Validate label name.
		errs := validation.IsQualifiedName(nameForValidation)
		if len(errs) > 0 {
			stderrLogger.Printf("Ignoring invalid feature name '%s': %s", label, errs)
			continue
		}

		value := fmt.Sprintf("%v", v)
		// Validate label value
		errs = validation.IsValidLabelValue(value)
		if len(errs) > 0 {
			stderrLogger.Printf("Ignoring invalid feature value %s=%s: %s", label, value, errs)
			continue
		}

		// Skip if label doesn't match labelWhiteList
		if !labelWhiteList.MatchString(nameForWhiteListing) {
			stderrLogger.Printf("%q does not match the whitelist (%s) and will not be published.", nameForWhiteListing, labelWhiteList.String())
			continue
		}

		labels[label] = value
	}
	return labels, nil
}

// advertiseFeatureLabels advertises the feature labels to a Kubernetes node
// via the NFD server.
func advertiseFeatureLabels(client pb.LabelerClient, labels Labels) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stdoutLogger.Printf("Sending labeling request to nfd-master")

	labelReq := pb.SetLabelsRequest{Labels: labels,
		NfdVersion: version.Get(),
		NodeName:   nodeName}
	_, err := client.SetLabels(ctx, &labelReq)
	if err != nil {
		stderrLogger.Printf("failed to set node labels: %v", err)
		return err
	}

	return nil
}

// UnmarshalJSON implements the Unmarshaler interface from "encoding/json"
func (d *duration) UnmarshalJSON(data []byte) error {
	var v interface{}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	switch val := v.(type) {
	case float64:
		d.Duration = time.Duration(val)
	case string:
		var err error
		d.Duration, err = time.ParseDuration(val)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("invalid duration %s", data)
	}
	return nil
}

// UnmarshalJSON implements the Unmarshaler interface from "encoding/json"
func (c *sourcesConfig) UnmarshalJSON(data []byte) error {
	// First do a raw parse to get the per-source data
	raw := map[string]json.RawMessage{}
	err := yaml.Unmarshal(data, &raw)
	if err != nil {
		return err
	}

	// Then parse each source-specific data structure
	// NOTE: we expect 'c' to be pre-populated with correct per-source data
	//       types. Non-pre-populated keys are ignored.
	for k, rawv := range raw {
		if v, ok := (*c)[k]; ok {
			err := yaml.Unmarshal(rawv, &v)
			if err != nil {
				return fmt.Errorf("failed to parse %q source config: %v", k, err)
			}
		}
	}

	return nil
}
