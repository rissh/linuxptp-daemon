package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/k8snetworkplumbingwg/linuxptp-daemon/pkg/synce"

	"github.com/k8snetworkplumbingwg/linuxptp-daemon/pkg/config"
	"github.com/k8snetworkplumbingwg/linuxptp-daemon/pkg/event"

	"github.com/golang/glog"

	ptpv1 "github.com/k8snetworkplumbingwg/ptp-operator/api/v1"
)

// LinuxPTPUpdate controls whether to update linuxPTP conf
// and contains linuxPTP conf to be updated. It's rendered
// and passed to linuxptp instance by daemon.
type LinuxPTPConfUpdate struct {
	UpdateCh               chan bool
	NodeProfiles           []ptpv1.PtpProfile
	appliedNodeProfileJson []byte
	defaultPTP4lConfig     []byte
}

type ptp4lConfSection struct {
	sectionName string
	options     map[string]string
}

type ptp4lConf struct {
	sections         []ptp4lConfSection
	mapping          []string
	profile_name     string
	clock_type       event.ClockType
	gnss_serial_port string // gnss serial port
}

func NewLinuxPTPConfUpdate() (*LinuxPTPConfUpdate, error) {
	if _, err := os.Stat(PTP4L_CONF_FILE_PATH); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("ptp.conf file doesn't exist")
		} else {
			return nil, fmt.Errorf("unknow error searching for the %s file: %v", PTP4L_CONF_FILE_PATH, err)
		}
	}

	defaultPTP4lConfig, err := os.ReadFile(PTP4L_CONF_FILE_PATH)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %v", PTP4L_CONF_FILE_PATH, err)
	}

	return &LinuxPTPConfUpdate{UpdateCh: make(chan bool), defaultPTP4lConfig: defaultPTP4lConfig}, nil
}

func (l *LinuxPTPConfUpdate) UpdateConfig(nodeProfilesJson []byte) error {
	if string(l.appliedNodeProfileJson) == string(nodeProfilesJson) {
		return nil
	}
	if nodeProfiles, ok := tryToLoadConfig(nodeProfilesJson); ok {
		glog.Info("load profiles")
		l.appliedNodeProfileJson = nodeProfilesJson
		l.NodeProfiles = nodeProfiles
		l.UpdateCh <- true

		return nil
	}

	if nodeProfiles, ok := tryToLoadOldConfig(nodeProfilesJson); ok {
		// Support empty old config
		// '{"name":null,"interface":null}'
		if nodeProfiles[0].Name == nil || nodeProfiles[0].Interface == nil {
			glog.Infof("Skip no profile %+v", nodeProfiles[0])
			return nil
		}

		glog.Info("load profiles using old method")
		l.appliedNodeProfileJson = nodeProfilesJson
		l.NodeProfiles = nodeProfiles
		l.UpdateCh <- true

		return nil
	}

	return fmt.Errorf("unable to load profile config")
}

// Try to load the multiple policy config
func tryToLoadConfig(nodeProfilesJson []byte) ([]ptpv1.PtpProfile, bool) {
	ptpConfig := []ptpv1.PtpProfile{}
	err := json.Unmarshal(nodeProfilesJson, &ptpConfig)
	if err != nil {
		return nil, false
	}

	return ptpConfig, true
}

// For backward compatibility we also try to load the one policy scenario
func tryToLoadOldConfig(nodeProfilesJson []byte) ([]ptpv1.PtpProfile, bool) {
	ptpConfig := &ptpv1.PtpProfile{}
	err := json.Unmarshal(nodeProfilesJson, ptpConfig)
	if err != nil {
		return nil, false
	}

	return []ptpv1.PtpProfile{*ptpConfig}, true
}

// Takes as input a PtpProfile.Ptp4lConf and outputs as ptp4lConf struct
func (output *ptp4lConf) populatePtp4lConf(config *string) error {
	var currentSectionName string
	var currentSection ptp4lConfSection
	output.sections = make([]ptp4lConfSection, 0)
	globalIsDefined := false
	hasSlaveConfigDefined := false

	if config != nil {
		for _, line := range strings.Split(*config, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "#") {
				continue
			} else if strings.HasPrefix(line, "[") {
				if currentSectionName != "" {
					output.sections = append(output.sections, currentSection)
				}
				currentLine := strings.Split(line, "]")

				if len(currentLine) < 2 {
					return errors.New("Section missing closing ']': " + line)
				}

				currentSectionName = fmt.Sprintf("%s]", currentLine[0])
				if currentSectionName == "[global]" {
					globalIsDefined = true
				}
				currentSection = ptp4lConfSection{options: map[string]string{}, sectionName: currentSectionName}
			} else if currentSectionName != "" {
				split := strings.IndexByte(line, ' ')
				if split > 0 {
					currentSection.options[line[:split]] = line[split:]
					if (line[:split] == "masterOnly" && line[split:] == "0") ||
						(line[:split] == "serverOnly" && line[split:] == "0") ||
						(line[:split] == "slaveOnly" && line[split:] == "1") ||
						(line[:split] == "clientOnly" && line[split:] == "1") {
						hasSlaveConfigDefined = true
					}
				}
			} else {
				return errors.New("Config option not in section: " + line)
			}
		}
		if currentSectionName != "" {
			output.sections = append(output.sections, currentSection)
		}
	}

	if !globalIsDefined {
		output.sections = append(output.sections, ptp4lConfSection{options: map[string]string{}, sectionName: "[global]"})
	}

	if !hasSlaveConfigDefined {
		// No Slave Interfaces defined
		output.clock_type = event.GM
	} else if len(output.sections) > 2 {
		// Multiple interfaces with at least one slave Interface defined
		output.clock_type = event.BC
	} else {
		// Single slave Interface defined
		output.clock_type = event.OC
	}
	return nil
}

func getSource(isTs2phcMaster string) event.EventSource {
	if ts2phcMaster, err := strconv.ParseBool(strings.TrimSpace(isTs2phcMaster)); err == nil {
		if ts2phcMaster {
			return event.GNSS
		}
	}
	return event.PPS
}

// extractSynceRelations extracts relation of synce device to interfaces
// The sections are ordered in the following way:
//  1. Device section specifies the configuration of a one logical device e.g. 'synce1'.
//     The name must be enclosed in extra angle bracket when defining new device section e.g. [<synce1>]
//     All ports defined by port sections AFTER the device section will create one SyncE device
//     (UNTIL next device section).
//  2. Port section - any other section not starting with < (e.g. [eth0]) is the port section.
//     Multiple port sections are allowed. Each port participates in SyncE communication.
func (conf *ptp4lConf) extractSynceRelations() *synce.Relations {
	var err error
	r := &synce.Relations{
		Devices: []*synce.Config{},
	}

	ifaces := []string{}
	re, _ := regexp.Compile(`[{}<>\[\] ]+`)
	synceRelationInfo := synce.Config{}

	var extendedTlv, networkOption int = synce.ExtendedTLV_DISABLED, synce.SYNCE_NETWORK_OPT_1
	for _, section := range conf.sections {
		if strings.HasPrefix(section.sectionName, "[<") {
			if synceRelationInfo.Name != "" {
				if len(ifaces) > 0 {
					synceRelationInfo.Ifaces = ifaces
				}
				r.AddDeviceConfig(synceRelationInfo)
			}
			synceRelationInfo = synce.Config{
				Name:           "",
				Ifaces:         nil,
				ClockId:        "",
				NetworkOption:  synce.SYNCE_NETWORK_OPT_1,
				ExtendedTlv:    synce.ExtendedTLV_DISABLED,
				ExternalSource: "",
				LastQLState:    make(map[string]*synce.QualityLevelInfo),
				LastClockState: "",
			}
			extendedTlv, networkOption = synce.ExtendedTLV_DISABLED, synce.SYNCE_NETWORK_OPT_1

			synceRelationInfo.Name = re.ReplaceAllString(section.sectionName, "")
			if networkOptionStr, ok := section.options["network_option"]; ok {
				if networkOption, err = strconv.Atoi(strings.TrimSpace(networkOptionStr)); err != nil {
					glog.Errorf("error parsing `network_option`, setting network_option to default 1 : %s", err)
				}
			}
			if extendedTlvStr, ok := section.options["extended_tlv"]; ok {
				if extendedTlv, err = strconv.Atoi(strings.TrimSpace(extendedTlvStr)); err != nil {
					glog.Errorf("error parsing `extended_tlv`, setting extended_tlv to default 1 : %s", err)
				}
			}
			synceRelationInfo.NetworkOption = networkOption
			synceRelationInfo.ExtendedTlv = extendedTlv
		} else if strings.HasPrefix(section.sectionName, "[{") {
			synceRelationInfo.ExternalSource = re.ReplaceAllString(section.sectionName, "")
		} else if strings.HasPrefix(section.sectionName, "[") && section.sectionName != "[global]" {
			iface := re.ReplaceAllString(section.sectionName, "")
			ifaces = append(ifaces, iface)
		}
	}
	if len(ifaces) > 0 {
		synceRelationInfo.Ifaces = ifaces
	}
	if synceRelationInfo.Name != "" {
		r.AddDeviceConfig(synceRelationInfo)
	}
	return r
}

func (conf *ptp4lConf) renderSyncE4lConf(ptpSettings map[string]string) (configOut string, relations *synce.Relations) {
	configOut = fmt.Sprintf("#profile: %s\n", conf.profile_name)
	relations = conf.extractSynceRelations()
	relations.AddClockIds(ptpSettings)
	deviceIdx := 0
	for _, section := range conf.sections {
		configOut = fmt.Sprintf("%s\n%s", configOut, section.sectionName)
		if strings.HasPrefix(section.sectionName, "[<") {
			if _, found := section.options["clock_id"]; !found {
				section.options["clock_id"] = relations.Devices[deviceIdx].ClockId
				deviceIdx++
			}
		}
		for k, v := range section.options {
			configOut = fmt.Sprintf("%s\n%s %s", configOut, k, v)
		}
	}
	return
}

func (conf *ptp4lConf) renderPtp4lConf() (configOut string, ifaces config.IFaces) {
	configOut = fmt.Sprintf("#profile: %s\n", conf.profile_name)
	conf.mapping = nil
	var nmea_source event.EventSource

	for _, section := range conf.sections {
		configOut = fmt.Sprintf("%s\n%s", configOut, section.sectionName)

		if section.sectionName == "[nmea]" {
			if source, ok := section.options["ts2phc.master"]; ok {
				nmea_source = getSource(source)
			}
		}
		if section.sectionName != "[global]" && section.sectionName != "[nmea]" {
			i := section.sectionName
			i = strings.ReplaceAll(i, "[", "")
			i = strings.ReplaceAll(i, "]", "")
			conf.mapping = append(conf.mapping, i)
			iface := config.Iface{Name: i}
			if source, ok := section.options["ts2phc.master"]; ok {
				iface.Source = getSource(source)
			} else {
				// if not defined here, use source defined at nmea section
				iface.Source = nmea_source
			}
			if masterOnly, ok := section.options["masterOnly"]; ok {
				// TODO add error handling
				iface.IsMaster, _ = strconv.ParseBool(strings.TrimSpace(masterOnly))
			}
			ifaces = append(ifaces, config.Iface{
				Name:   iface.Name,
				Source: iface.Source,
				PhcId:  iface.PhcId,
			})
		}
		for k, v := range section.options {
			configOut = fmt.Sprintf("%s\n%s %s", configOut, k, v)
		}
	}
	return configOut, ifaces
}
