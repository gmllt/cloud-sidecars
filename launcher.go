package sidecars

import (
	"fmt"
	"github.com/gliderlabs/sigil"
	"github.com/olekukonko/tablewriter"
	"github.com/orange-cloudfoundry/cloud-sidecars/config"
	"github.com/orange-cloudfoundry/cloud-sidecars/starter"
	"github.com/orange-cloudfoundry/cloud-sidecars/utils"
	log "github.com/sirupsen/logrus"
	"gopkg.in/alessio/shellescape.v1"
	"io"
	"io/ioutil"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

func init() {
	sigil.PosixPreprocess = true
}

const (
	ProxyAppPortEnvKey = "PROXY_APP_PORT"
	AppPortEnvKey      = "SIDECAR_APP_PORT"
	PathSidecarsWd     = ".sidecars"
)

type Launcher struct {
	sConfig    config.Sidecars
	cStarter   starter.Starter
	profileDir string
	stdout     io.Writer
	stderr     io.Writer
	appPort    int
}

func NewLauncher(
	sConfig config.Sidecars,
	cStarter starter.Starter,
	profileDir string,
	stdout, stderr io.Writer,
	defaultAppPort int,
) *Launcher {
	var appPort int
	if cStarter != nil && !sConfig.NoStarter {
		appPort = cStarter.AppPort()
	}
	if appPort == 0 {
		appPort = sConfig.AppPort
	}
	if appPort == 0 {
		appPort = defaultAppPort
	}
	return &Launcher{
		sConfig:    sConfig,
		cStarter:   cStarter,
		profileDir: profileDir,
		stdout:     stdout,
		stderr:     stderr,
		appPort:    appPort,
	}
}

func (l Launcher) ShowSidecarsSha1() error {
	table := tablewriter.NewWriter(l.stdout)
	table.SetHeader([]string{"Sidecar Name", "Sha1"})
	for _, sidecar := range l.sConfig.Sidecars {
		if sidecar.ArtifactURI == "" {
			table.Append([]string{sidecar.Name, "-"})
			continue
		}
		s, err := ZipperSess(sidecar.ArtifactURI, sidecar.ArtifactType)
		if err != nil {
			return NewSidecarError(sidecar, err)
		}
		sha1, err := s.Sha1()
		if err != nil {
			return NewSidecarError(sidecar, err)
		}
		table.Append([]string{sidecar.Name, sha1})
	}
	table.Render()
	return nil
}

func (l Launcher) Setup(force bool) error {
	entryG := log.WithField("component", "Launcher").WithField("command", "staging")
	entryG.Infof("Setup sidecars ...")
	appEnv := make(map[string]string)
	err := os.MkdirAll(l.profileDir, 0755)
	if err != nil {
		return err
	}
	err = l.DownloadArtifacts(force)
	if err != nil {
		return err
	}
	appPort := l.appPort
	for index, sidecar := range l.sConfig.Sidecars {
		entry := entryG.WithField("sidecar", sidecar.Name)
		entry.Infof("Setup ...")
		appEnvUnTpl, err := TemplatingEnv(appEnv, sidecar.AppEnv)
		if err != nil {
			return err
		}
		appEnv = utils.MergeEnv(appEnv, appEnvUnTpl)
		if sidecar.IsRproxy {
			appPort++
		}
		if sidecar.ProfileD != "" {
			entry.Infof("Writing profiled file ...")
			err := ioutil.WriteFile(
				filepath.Join(l.profileDir, fmt.Sprintf("%d_%s.sh", index+1, sidecar.Name)),
				[]byte(sidecar.ProfileD), 0755)
			if err != nil {
				return err
			}
			entry.Infof("Finished writing profiled file.")
		}

		entry.Infof("Finished setup.")
	}
	entryG.Infof("Finished setup sidecars.")
	if l.cStarter == nil || l.sConfig.NoStarter {
		return nil
	}
	if appPort != l.appPort {
		appEnv = utils.MergeEnv(appEnv, l.cStarter.ProxyEnv(appPort))
		appEnv = utils.MergeEnv(appEnv, map[string]string{
			AppPortEnvKey: strconv.Itoa(l.appPort),
		})
	}
	entryG.WithField("starter", l.cStarter.CloudEnvName()).Info("Adding starter.sh profile")
	profileLaunch := ""
	for k, v := range appEnv {
		profileLaunch += fmt.Sprintf("export %s=%s\n", k, shellescape.Quote(v))
	}
	err = ioutil.WriteFile(
		filepath.Join(l.profileDir, "0_starter.sh"),
		[]byte(profileLaunch), 0755)
	if err != nil {
		return err
	}
	entryG.WithField("starter", l.cStarter.CloudEnvName()).Info("Finished adding starter.sh profile")
	return nil
}

func (l Launcher) DownloadArtifacts(forceDl bool) error {
	entryG := log.WithField("component", "Launcher").WithField("command", "download_artifact")
	entryG.Info("Start downloading artifacts from sidecars ...")
	for _, sidecar := range l.sConfig.Sidecars {
		if sidecar.ArtifactURI == "" {
			continue
		}
		entry := entryG.WithField("sidecar", sidecar.Name)
		dir := SidecarDir(l.sConfig.Dir, sidecar.Name)
		err := os.MkdirAll(dir, os.ModePerm)
		if err != nil {
			return NewSidecarError(sidecar, err)
		}
		isEmpty, err := IsEmptyDir(dir)
		if err != nil {
			return NewSidecarError(sidecar, err)
		}
		if !isEmpty && !forceDl {
			entry.Infof("Skipping downloading from %s (directory not empty, sidecar must be already downloaded)", sidecar.ArtifactURI)
			return nil
		}
		if !isEmpty {
			err := os.RemoveAll(dir)
			if err != nil {
				return NewSidecarError(sidecar, err)
			}
			err = os.MkdirAll(dir, os.ModePerm)
			if err != nil {
				return NewSidecarError(sidecar, err)
			}
		}
		err = DownloadSidecar(dir, sidecar)
		if err != nil {
			return NewSidecarError(sidecar, err)
		}

		if sidecar.AfterDownload == "" {
			continue
		}

		entry.Info("Run after install script ...")
		env, err := OverrideEnv(utils.OsEnvToMap(), sidecar.Env)
		if err != nil {
			return NewSidecarError(sidecar, err)
		}
		err = runScript(
			sidecar.AfterDownload,
			filepath.Dir(SidecarExecPath(l.sConfig.Dir, sidecar)),
			utils.EnvMapToOsEnv(env),
			l.stdout, l.stderr,
		)
		if err != nil {
			return NewSidecarError(sidecar, err)
		}
		entry.Info("Finished running after install script.")
	}
	entryG.Info("Finished downloading artifacts from sidecars.")
	return nil
}

func (l Launcher) Launch() error {
	entryG := log.WithField("component", "Launcher").
		WithField("command", "launch")
	appEnv := utils.OsEnvToMap()

	wg := &sync.WaitGroup{}
	processLen := len(l.sConfig.Sidecars)
	if !l.sConfig.NoStarter {
		processLen++
	}
	wg.Add(processLen)

	processes := make([]*process, processLen)
	pProcesses := &processes

	errChan := make(chan error, 100)
	signalChan := make(chan os.Signal, 100)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL)

	var cStarter starter.Starter = nil
	if !l.sConfig.NoStarter {
		cStarter = l.cStarter
	}
	factory := NewProcessFactory(errChan, signalChan, wg, l.stdout, l.stderr, cStarter, l.sConfig.Dir)

	var err error
	i := 0
	appPort := l.appPort
	if os.Getenv(AppPortEnvKey) != "" {
		appPort, err = strconv.Atoi(os.Getenv(AppPortEnvKey))
		if err != nil {
			return err
		}
	}
	for _, sidecar := range l.sConfig.Sidecars {
		env, err := OverrideEnv(utils.OsEnvToMap(), sidecar.Env)
		if err != nil {
			return NewSidecarError(sidecar, err)
		}
		if sidecar.IsRproxy {
			if l.cStarter != nil && !l.sConfig.NoStarter {
				env, err = OverrideEnv(env, l.cStarter.ProxyEnv(appPort))
				if err != nil {
					return NewSidecarError(sidecar, err)
				}
			}
			appPort++
			env, err = OverrideEnv(env, map[string]string{
				ProxyAppPortEnvKey: fmt.Sprintf("%d", appPort),
			})
			if err != nil {
				return NewSidecarError(sidecar, err)
			}
		}
		entry := entryG.WithField("sidecar", sidecar.Name)
		entry.Debug("Setup sidecar ...")
		appEnvUnTpl, err := TemplatingEnv(appEnv, sidecar.AppEnv)
		if err != nil {
			return NewSidecarError(sidecar, err)
		}
		appEnv = utils.MergeEnv(appEnv, appEnvUnTpl)
		processes[i], err = factory.FromSidecar(sidecar, env)
		if err != nil {
			return NewSidecarError(sidecar, err)
		}
		i++

		entry.Debug("Finished setup sidecar.")
	}

	// manage graceful shutdown
	go l.handlingSignal(pProcesses, processLen, signalChan)

	if !l.sConfig.NoStarter {
		entryS := entryG.WithField("starter", l.cStarter.CloudEnvName())
		if appPort != l.appPort {
			appEnv = utils.MergeEnv(appEnv, l.cStarter.ProxyEnv(appPort))
		}
		entryS.Debug("Setup cloud starter ...")
		processes[i], err = factory.FromStarter(appEnv, l.profileDir)
		if err != nil {
			return err
		}
		entryS.Debug("Finished setup cloud starter ...")
	}

	for _, p := range processes {
		go p.Start()
	}
	wg.Wait()
	select {
	case err = <-errChan:
		return err
	default:
		return nil
	}
	return nil
}

func (l Launcher) handlingSignal(pProcesses *[]*process, processLen int, signalChan chan os.Signal) {
	sig := <-signalChan
	// If signal has been set by other process at init we are waiting
	// to reach number of process required before sending back signal
	for !processesNotHaveLen(*pProcesses, processLen) {
		time.Sleep(10 * time.Millisecond)
	}
	for _, process := range *pProcesses {
		if process.cmd.Process == nil {
			continue // process is not running (which probably create signal)
		}
		// resent signal for each process to make them detect
		// when they receive a signal to not show error
		signalChan <- sig
		// if setpgid exist in sysproc, we need to send signal to negative pid (-pid)
		// we override pid value to let us use process.Process.Signal
		// instead of non os agnostic syscall funcs
		if utils.HasPgidSysProcAttr(process.cmd.SysProcAttr) {
			process.cmd.Process.Pid = -process.cmd.Process.Pid
		}
		process.cmd.Process.Signal(sig)
	}
	// if processes still doesn't stop after 20 sec we force shutdown
	time.Sleep(20 * time.Second)
	for _, process := range *pProcesses {
		signalChan <- syscall.SIGKILL
		process.cmd.Process.Kill()
	}
}

func SidecarDir(baseDir, sidecarName string) string {
	return filepath.Join(baseDir, PathSidecarsWd, sidecarName)
}

func processesNotHaveLen(processes []*process, len int) bool {
	var i int
	var p *process
	for i, p = range processes {
		if p == nil {
			break
		}
	}
	return (i + 1) == len
}
