//
// (C) Copyright 2019-2021 Intel Corporation.
//
// SPDX-License-Identifier: BSD-2-Clause-Patent
//

package bdev

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/pkg/errors"

	"github.com/daos-stack/daos/src/control/lib/spdk"
	"github.com/daos-stack/daos/src/control/logging"
	"github.com/daos-stack/daos/src/control/server/storage"
)

const (
	hugePageDir    = "/dev/hugepages"
	hugePagePrefix = "spdk"
)

type (
	spdkWrapper struct {
		spdk.Env
		spdk.Nvme

		vmdDisabled bool
	}

	spdkBackend struct {
		log     logging.Logger
		binding *spdkWrapper
		script  *spdkSetupScript
	}

	removeFn func(string) error
)

// suppressOutput is a horrible, horrible hack necessitated by the fact that
// SPDK blathers to stdout, causing console spam and messing with our secure
// communications channel between the server and privileged helper.

func (w *spdkWrapper) suppressOutput() (restore func(), err error) {
	realStdout, dErr := syscall.Dup(syscall.Stdout)
	if dErr != nil {
		err = dErr
		return
	}

	devNull, oErr := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if oErr != nil {
		err = oErr
		return
	}

	if err = syscall.Dup2(int(devNull.Fd()), syscall.Stdout); err != nil {
		return
	}

	restore = func() {
		// NB: Normally panic() in production code is frowned upon, but in this
		// case if we get errors there really isn't any handling to be done
		// because things have gone completely sideways.
		if err := devNull.Close(); err != nil {
			panic(err)
		}
		if err := syscall.Dup2(realStdout, syscall.Stdout); err != nil {
			panic(err)
		}
	}

	return
}

func (w *spdkWrapper) init(log logging.Logger, spdkOpts *spdk.EnvOptions) (func(), error) {
	restore, err := w.suppressOutput()
	if err != nil {
		return nil, errors.Wrap(err, "failed to suppress spdk output")
	}

	if err := w.InitSPDKEnv(log, spdkOpts); err != nil {
		restore()
		return nil, errors.Wrap(err, "failed to init spdk env")
	}

	return restore, nil
}

func newBackend(log logging.Logger, sr *spdkSetupScript) *spdkBackend {
	return &spdkBackend{
		log:     log,
		binding: &spdkWrapper{Env: &spdk.EnvImpl{}, Nvme: &spdk.NvmeImpl{}},
		script:  sr,
	}
}

func defaultBackend(log logging.Logger) *spdkBackend {
	return newBackend(log, defaultScriptRunner(log))
}

// DisableVMD turns off VMD device awareness.
func (b *spdkBackend) DisableVMD() {
	b.binding.vmdDisabled = true
}

// IsVMDDisabled checks for VMD device awareness.
func (b *spdkBackend) IsVMDDisabled() bool {
	return b.binding.vmdDisabled
}

// Scan discovers NVMe controllers accessible by SPDK.
func (b *spdkBackend) Scan(req ScanRequest) (*ScanResponse, error) {
	restoreOutput, err := b.binding.init(b.log, &spdk.EnvOptions{
		PciIncludeList: req.DeviceList,
		DisableVMD:     b.IsVMDDisabled(),
	})
	if err != nil {
		return nil, err
	}
	defer restoreOutput()

	cs, err := b.binding.Discover(b.log)
	if err != nil {
		return nil, errors.Wrap(err, "failed to discover nvme")
	}

	return &ScanResponse{Controllers: cs}, nil
}

func (b *spdkBackend) formatRespFromResults(results []*spdk.FormatResult) (*FormatResponse, error) {
	resp := &FormatResponse{
		DeviceResponses: make(DeviceFormatResponses),
	}
	resultMap := make(map[string]map[int]error)

	// build pci address to namespace errors map
	for _, result := range results {
		if _, exists := resultMap[result.CtrlrPCIAddr]; !exists {
			resultMap[result.CtrlrPCIAddr] = make(map[int]error)
		}

		if _, exists := resultMap[result.CtrlrPCIAddr][int(result.NsID)]; exists {
			return nil, errors.Errorf("duplicate error for ns %d on %s",
				result.NsID, result.CtrlrPCIAddr)
		}

		resultMap[result.CtrlrPCIAddr][int(result.NsID)] = result.Err
	}

	// populate device responses for failed/formatted namespacess
	for addr, nsErrMap := range resultMap {
		var formatted, failed, all []int
		var firstErr error

		for nsID := range nsErrMap {
			all = append(all, nsID)
		}
		sort.Ints(all)
		for _, nsID := range all {
			err := nsErrMap[nsID]
			if err != nil {
				failed = append(failed, nsID)
				if firstErr == nil {
					firstErr = errors.Wrapf(err, "namespace %d", nsID)
				}
				continue
			}
			formatted = append(formatted, nsID)
		}

		b.log.Debugf("formatted namespaces %v on nvme device at %s", formatted, addr)

		devResp := new(DeviceFormatResponse)
		if firstErr != nil {
			devResp.Error = FaultFormatError(addr, errors.Errorf(
				"failed to format namespaces %v (%s)",
				failed, firstErr))
			resp.DeviceResponses[addr] = devResp
			continue
		}

		devResp.Formatted = true
		resp.DeviceResponses[addr] = devResp
	}

	return resp, nil
}

func (b *spdkBackend) formatNvme(req FormatRequest) (*FormatResponse, error) {
	spdkOpts := &spdk.EnvOptions{
		MemSize:        req.MemSize,
		PciIncludeList: req.DeviceList,
		DisableVMD:     b.IsVMDDisabled(),
	}

	restoreOutput, err := b.binding.init(b.log, spdkOpts)
	if err != nil {
		return nil, err
	}
	defer restoreOutput()
	defer b.binding.FiniSPDKEnv(b.log, spdkOpts)
	defer func() {
		if err := b.binding.CleanLockfiles(b.log, req.DeviceList...); err != nil {
			b.log.Errorf("cleanup failed after format: %s", err)
		}
	}()

	results, err := b.binding.Format(b.log)
	if err != nil {
		return nil, errors.Wrapf(err, "spdk format %v", req.DeviceList)
	}

	if len(results) == 0 {
		return nil, errors.New("empty results from spdk binding format request")
	}

	return b.formatRespFromResults(results)
}

// Format initializes the SPDK environment, defers the call to finalize the same
// environment and calls private format() routine to format all devices in the
// request device list in a manner specific to the supplied bdev class.
//
// Remove any stale SPDK lockfiles after format.
func (b *spdkBackend) Format(req FormatRequest) (*FormatResponse, error) {
	// TODO (DAOS-3844): Kick off device formats parallel?
	switch req.Class {
	case storage.BdevClassKdev, storage.BdevClassFile, storage.BdevClassMalloc:
		resp := &FormatResponse{
			DeviceResponses: make(DeviceFormatResponses),
		}

		for _, device := range req.DeviceList {
			resp.DeviceResponses[device] = new(DeviceFormatResponse)
			b.log.Debugf("%s format for non-NVMe bdev skipped on %s", req.Class, device)
		}

		return resp, nil
	case storage.BdevClassNvme:
		if len(req.DeviceList) == 0 {
			return nil, errors.New("empty pci address list in nvme format request")
		}

		return b.formatNvme(req)
	default:
		return nil, FaultFormatUnknownClass(req.Class.String())
	}
}

// detectVMD returns whether VMD devices have been found and a slice of VMD
// PCI addresses if found.
func detectVMD() ([]string, error) {
	// Check available VMD devices with command:
	// "$lspci | grep  -i -E "201d | Volume Management Device"
	lspciCmd := exec.Command("lspci")
	vmdCmd := exec.Command("grep", "-i", "-E", "201d|Volume Management Device")
	var cmdOut bytes.Buffer
	var prefixIncluded bool

	vmdCmd.Stdin, _ = lspciCmd.StdoutPipe()
	vmdCmd.Stdout = &cmdOut
	_ = lspciCmd.Start()
	_ = vmdCmd.Run()
	_ = lspciCmd.Wait()

	if cmdOut.Len() == 0 {
		return []string{}, nil
	}

	vmdCount := bytes.Count(cmdOut.Bytes(), []byte("0000:"))
	if vmdCount == 0 {
		// sometimes the output may not include "0000:" prefix
		// usually when muliple devices are in the PCI_WHITELIST
		vmdCount = bytes.Count(cmdOut.Bytes(), []byte("Volume"))
		if vmdCount == 0 {
			vmdCount = bytes.Count(cmdOut.Bytes(), []byte("201d"))
		}
	} else {
		prefixIncluded = true
	}
	vmdAddrs := make([]string, 0, vmdCount)

	i := 0
	scanner := bufio.NewScanner(&cmdOut)
	for scanner.Scan() {
		if i == vmdCount {
			break
		}
		s := strings.Split(scanner.Text(), " ")
		if !prefixIncluded {
			s[0] = "0000:" + s[0]
		}
		vmdAddrs = append(vmdAddrs, strings.TrimSpace(s[0]))
		i++
	}

	if len(vmdAddrs) == 0 {
		return nil, errors.New("error parsing cmd output")
	}

	return vmdAddrs, nil
}

// hugePageWalkFunc returns a filepath.WalkFunc that will remove any file whose
// name begins with prefix and owner has uid equal to tgtUid.
func hugePageWalkFunc(hugePageDir, prefix, tgtUid string, remove removeFn) filepath.WalkFunc {
	return func(path string, info os.FileInfo, err error) error {
		switch {
		case err != nil:
			return err
		case info == nil:
			return errors.New("nil fileinfo")
		case info.IsDir():
			if path == hugePageDir {
				return nil
			}
			return filepath.SkipDir // skip subdirectories
		case !strings.HasPrefix(info.Name(), prefix):
			return nil // skip files without prefix
		}

		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat == nil {
			return errors.New("stat missing for file")
		}
		if strconv.Itoa(int(stat.Uid)) != tgtUid {
			return nil // skip not owned by target user
		}

		if err := remove(path); err != nil {
			return err
		}

		return nil
	}
}

// cleanHugePages removes hugepage files with pathPrefix that are owned by the
// user with username tgtUsr by processing directory tree with filepath.WalkFunc
// returned from hugePageWalkFunc.
func cleanHugePages(hugePageDir, prefix, tgtUid string) error {
	return filepath.Walk(hugePageDir,
		hugePageWalkFunc(hugePageDir, prefix, tgtUid, os.Remove))
}

func (b *spdkBackend) vmdPrep(req PrepareRequest) (bool, error) {
	vmdDevs, err := detectVMD()
	if err != nil {
		return false, errors.Wrap(err, "VMD could not be enabled")
	}

	if len(vmdDevs) == 0 {
		return false, nil
	}

	vmdReq := req
	// If VMD devices are going to be used, then need to run a separate
	// bdev prepare (SPDK setup) with the VMD address as the PCI_WHITELIST
	//
	// TODO: ignore devices not in include list
	vmdReq.PCIAllowlist = strings.Join(vmdDevs, " ")

	if err := b.script.Prepare(vmdReq); err != nil {
		return false, errors.Wrap(err, "re-binding vmd ssds to attach with spdk")
	}

	b.log.Debugf("volume management devices detected: %v", vmdDevs)
	return true, nil
}

// Prepare will cleanup any leftover hugepages owned by the target user and then
// executes the SPDK setup.sh script to rebind PCI devices as selected by
// bdev_include and bdev_exclude list filters provided in the server config file.
// This will make the devices available though SPDK.
func (b *spdkBackend) Prepare(req PrepareRequest) (*PrepareResponse, error) {
	b.log.Debugf("provider backend prepare %v", req)
	resp := &PrepareResponse{}

	usr, err := user.Lookup(req.TargetUser)
	if err != nil {
		return nil, errors.Wrapf(err, "lookup on local host")
	}

	if err := b.script.Prepare(req); err != nil {
		return nil, errors.Wrap(err, "re-binding ssds to attach with spdk")
	}

	if !req.DisableCleanHugePages {
		// remove hugepages matching /dev/hugepages/spdk* owned by target user
		err := cleanHugePages(hugePageDir, hugePagePrefix, usr.Uid)
		if err != nil {
			return nil, errors.Wrapf(err, "clean spdk hugepages")
		}
	}

	if !req.DisableVMD {
		vmdDetected, err := b.vmdPrep(req)
		if err != nil {
			return nil, err
		}
		resp.VmdDetected = vmdDetected
	}

	return resp, nil
}

func (b *spdkBackend) PrepareReset() error {
	b.log.Debugf("provider backend prepare reset")
	return b.script.Reset()
}

func (b *spdkBackend) UpdateFirmware(pciAddr string, path string, slot int32) error {
	if pciAddr == "" {
		return FaultBadPCIAddr("")
	}

	restoreOutput, err := b.binding.init(b.log, &spdk.EnvOptions{
		DisableVMD: b.IsVMDDisabled(),
	})
	if err != nil {
		return err
	}
	defer restoreOutput()

	cs, err := b.binding.Discover(b.log)
	if err != nil {
		return errors.Wrap(err, "failed to discover nvme")
	}

	var found bool
	for _, c := range cs {
		if c.PciAddr == pciAddr {
			found = true
			break
		}
	}

	if !found {
		return FaultPCIAddrNotFound(pciAddr)
	}

	if err := b.binding.Update(b.log, pciAddr, path, slot); err != nil {
		return err
	}

	return nil
}
