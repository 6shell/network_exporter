package config

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/creasty/defaults"
	"github.com/syepes/network_exporter/pkg/common"

	yaml "gopkg.in/yaml.v3"
)

// Config represents configuration for the exporter

type Targets []struct {
	Name     string   `yaml:"name" json:"name"`
	Host     string   `yaml:"host" json:"host"`
	Type     string   `yaml:"type" json:"type"`
	Proxy    string   `yaml:"proxy" json:"proxy"`
	Probe    []string `yaml:"probe" json:"probe"`
	SourceIp string   `yaml:"source_ip" json:"source_ip"`
	Labels   extraKV  `yaml:"labels,omitempty" json:"labels,omitempty"`
}

type HTTPGet struct {
	Interval duration `yaml:"interval" json:"interval" default:"15s"`
	Timeout  duration `yaml:"timeout" json:"timeout" default:"14s"`
}

type TCP struct {
	Interval duration `yaml:"interval" json:"interval" default:"5s"`
	Timeout  duration `yaml:"timeout" json:"timeout" default:"4s"`
}

type MTR struct {
	Interval duration `yaml:"interval" json:"interval" default:"5s"`
	Timeout  duration `yaml:"timeout" json:"timeout" default:"4s"`
	MaxHops  int      `yaml:"max-hops" json:"max-hops" default:"30"`
	Count    int      `yaml:"count" json:"count" default:"10"`
}

type ICMP struct {
	Interval duration `yaml:"interval" json:"interval" default:"5s"`
	Timeout  duration `yaml:"timeout" json:"timeout" default:"4s"`
	Count    int      `yaml:"count" json:"count" default:"10"`
}

type Conf struct {
	Refresh           duration `yaml:"refresh" json:"refresh" default:"0s"`
	Nameserver        string   `yaml:"nameserver" json:"nameserver"`
	NameserverTimeout duration `yaml:"nameserver_timeout" json:"nameserver_timeout" default:"250ms"`
}

type Config struct {
	Conf    `yaml:"conf" json:"conf"`
	ICMP    `yaml:"icmp" json:"icmp"`
	MTR     `yaml:"mtr" json:"mtr"`
	TCP     `yaml:"tcp" json:"tcp"`
	HTTPGet `yaml:"http_get" json:"http_get"`
	Targets `yaml:"targets" json:"targets"`
}

type duration time.Duration

type extraKV struct {
	Kv map[string]string `yaml:"kv,omitempty" json:"kv,omitempty"`
}

// UnmarshalYAML is used to unmarshal into map[string]string
func (b *extraKV) UnmarshalYAML(unmarshal func(interface{}) error) error {
	return unmarshal(&b.Kv)
}

// SafeConfig Safe configuration reload
type Resolver struct {
	Resolver *net.Resolver
	Timeout  time.Duration
}

// SafeConfig Safe configuration reload
type SafeConfig struct {
	Cfg *Config
	sync.RWMutex
}

// ReloadConfig Safe configuration reload
func (sc *SafeConfig) ReloadConfig(logger *slog.Logger, confFile string) (err error) {
	hostname, err := os.Hostname()
	if err != nil {
		panic(err)
	}

	var c = &Config{}
	f, err := os.Open(confFile)
	if err != nil {
		return fmt.Errorf("reading config file: %s", err)
	}
	defer f.Close()

	decoder := yaml.NewDecoder(f)
	if err = decoder.Decode(c); err != nil {
		return fmt.Errorf("parsing config file: %s", err)
	}

	if err := defaults.Set(c); err != nil {
		return fmt.Errorf("setting defaults: %s", err)
	}

	// Validate and Filter config
	targets := Targets{}
	re := regexp.MustCompile("^ICMP|MTR|ICMP+MTR|TCP|HTTPGet$")
	for _, t := range c.Targets {
		if common.SrvRecordCheck(t.Host) {
			found := re.MatchString(t.Type)
			if !found {
				logger.Error("Unknown check type", "type", "Config", "func", "ReloadConfig", "target", t.Name, "check_type", t.Type, "allowed", "(ICMP|MTR|ICMP+MTR|TCP|HTTPGet)")
				continue
			}
			// Check that SRV record's type is TCP, if config's type is TCP
			if t.Type == "TCP" {
				if !strings.EqualFold(t.Type, strings.Split(t.Host, ".")[1][1:]) {
					logger.Error("Target type doesn't match SRV record protocol", "type", "Config", "func", "ReloadConfig", "target", t.Name, "check_type", t.Type, "srv_proto", strings.Split(t.Host, ".")[1][1:])
					continue
				}
			}

			srv_record_hosts, err := common.SrvRecordHosts(t.Host)
			if err != nil {
				logger.Error("Error processing SRV record", "type", "Config", "func", "ReloadConfig", "target", t.Host, "err", err)
				continue
			}

			for _, srvTarget := range srv_record_hosts {
				sub_target := t
				sub_target.Name = srvTarget
				sub_target.Host = srvTarget

				// Filter out the targets that are not assigned to the running host, if the `probe` is not specified don't filter
				if sub_target.Probe == nil {
					targets = append(targets, sub_target)
				} else {
					for _, p := range sub_target.Probe {
						if p == hostname {
							targets = append(targets, sub_target)
							continue
						}
					}
				}
			}
		} else {
			found := re.MatchString(t.Type)
			if !found {
				logger.Error("Unknown check type", "type", "Config", "func", "ReloadConfig", "target", t.Name, "check_type", t.Type, "allowed", "(ICMP|MTR|ICMP+MTR|TCP|HTTPGet)")
				continue
			}

			// Filter out the targets that are not assigned to the running host, if the `probe` is not specified don't filter
			if t.Probe == nil {
				targets = append(targets, t)
			} else {
				for _, p := range t.Probe {
					if p == hostname {
						targets = append(targets, t)
						continue
					}
				}
			}
		}
	}

	// Remap the filtered targets
	c.Targets = targets

	if _, err = HasDuplicateTargets(c.Targets); err != nil {
		return fmt.Errorf("parsing config file: %s", err)
	}

	// Config precheck
	if c.ICMP.Interval <= 0 || c.MTR.Interval <= 0 || c.TCP.Interval <= 0 || c.HTTPGet.Interval <= 0 {
		return fmt.Errorf("intervals (icmp,mtr,tcp,http_get) must be >0")
	}
	if c.MTR.MaxHops < 0 || c.MTR.MaxHops > 65500 {
		return fmt.Errorf("mtr.max-hops must be between 0 and 65500")
	}
	if c.MTR.Count < 0 || c.MTR.Count > 65500 {
		return fmt.Errorf("mtr.count must be between 0 and 65500")
	}

	sc.Lock()
	sc.Cfg = c
	sc.Unlock()

	return nil
}

// UnmarshalYAML implements yaml.Unmarshaler interface.
func (d *duration) UnmarshalYAML(unmashal func(interface{}) error) error {
	var s string
	if err := unmashal(&s); err != nil {
		return err
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = duration(dur)
	return nil
}

// Duration is a convenience getter.
func (d duration) Duration() time.Duration {
	return time.Duration(d)
}

// Set updates the underlying duration.
func (d *duration) Set(dur time.Duration) {
	*d = duration(dur)
}

// HasDuplicateTargets Find duplicates with same type
func HasDuplicateTargets(m Targets) (bool, error) {
	tmp := map[string]map[string]bool{
		"TCP":     map[string]bool{},
		"ICMP":    map[string]bool{},
		"MTR":     map[string]bool{},
		"HTTPGet": map[string]bool{},
	}

	for _, t := range m {
		if t.Type == "ICMP+MTR" {
			if tmp["MTR"][t.Name] {
				return true, fmt.Errorf("found duplicated record: %s", t.Name)
			}
			tmp["MTR"][t.Name] = true
			if tmp["ICMP"][t.Name] {
				return true, fmt.Errorf("found duplicated record: %s", t.Name)
			}
			tmp["ICMP"][t.Name] = true
		} else {
			if tmp[t.Type][t.Name] {
				return true, fmt.Errorf("found duplicated record: %s", t.Name)
			}
			tmp[t.Type][t.Name] = true
		}
	}
	return false, nil
}