//
// (C) Copyright 2020-2021 Intel Corporation.
//
// SPDX-License-Identifier: BSD-2-Clause-Patent
//

package main

import (
	"context"
	"strings"

	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"

	"github.com/daos-stack/daos/src/control/cmd/dmg/pretty"
	"github.com/daos-stack/daos/src/control/lib/control"
	"github.com/daos-stack/daos/src/control/lib/netdetect"
)

// configCmd is the struct representing the top-level config subcommand.
type configCmd struct {
	Generate configGenCmd `command:"generate" alias:"g" description:"Generate DAOS server configuration file based on discoverable hardware devices"`
}

type configGenCmd struct {
	logCmd
	cfgCmd
	ctlInvokerCmd
	hostListCmd
	jsonOutputCmd
	AccessPoints string `short:"a" long:"access-points" description:"Comma separated list of access point addresses <ipv4addr/hostname>"`
	NrEngines    int    `short:"e" long:"num-engines" description:"Set the number of DAOS Engine sections to be populated in the config file output. If unset then the value will be set to the number of NUMA nodes on storage hosts in the DAOS system."`
	MinNrSSDs    int    `default:"1" short:"s" long:"min-ssds" description:"Minimum number of NVMe SSDs required per DAOS Engine (SSDs must reside on the host that is managing the engine). Set to 0 to generate a config with no NVMe."`
	NetClass     string `default:"best-available" short:"c" long:"net-class" description:"Network class preferred" choice:"best-available" choice:"ethernet" choice:"infiniband"`
}

// Execute is run when configGenCmd activates.
//
// Attempt to auto generate a server config file with populated storage and
// network hardware parameters suitable to be used on all hosts in provided host
// list.
func (cmd *configGenCmd) Execute(_ []string) error {
	ctx := context.Background()

	cmd.log.Debugf("configGenCmd input control config: %+v", cmd.config)

	req := control.ConfigGenerateReq{
		NrEngines: cmd.NrEngines,
		MinNrSSDs: cmd.MinNrSSDs,
		HostList:  cmd.config.HostList,
		Client:    cmd.ctlInvoker,
		Log:       cmd.log,
	}
	switch cmd.NetClass {
	case "ethernet":
		req.NetClass = netdetect.Ether
	case "infiniband":
		req.NetClass = netdetect.Infiniband
	default:
		req.NetClass = control.NetDevAny
	}
	if cmd.AccessPoints != "" {
		req.AccessPoints = strings.Split(cmd.AccessPoints, ",")
	}

	// TODO: decide whether we want meaningful JSON output
	if cmd.jsonOutputEnabled() {
		return cmd.outputJSON(new(control.ConfigGenerateResp), nil)
	}

	resp, err := control.ConfigGenerate(ctx, req)
	if resp == nil {
		if err == nil {
			return errors.New("nil response from config generate")
		}
		return err
	}

	if resp.Errors() != nil {
		// host level errors e.g. unresponsive daos_server process
		var bld strings.Builder
		if err := pretty.PrintResponseErrors(resp, &bld); err != nil {
			return err
		}
		cmd.log.Error(bld.String()) // no-op if no host level errors
	}

	// includes hardware validation errors e.g. hardware across hostset differs
	if err != nil {
		return err
	}

	bytes, err := yaml.Marshal(resp.ConfigOut)
	if err != nil {
		return err
	}

	// output recommended server config yaml file
	cmd.log.Info(string(bytes))
	return nil
}
