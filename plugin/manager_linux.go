package plugin

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/daemon/initlayer"
	"github.com/docker/docker/pkg/containerfs"
	"github.com/docker/docker/pkg/idtools"
	"github.com/docker/docker/pkg/mount"
	"github.com/docker/docker/pkg/plugins"
	"github.com/docker/docker/pkg/stringid"
	"github.com/docker/docker/plugin/v2"
	"github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

func (pm *Manager) enable(p *v2.Plugin, c *controller, force bool) (err error) {
	p.Rootfs = filepath.Join(pm.config.Root, p.PluginObj.ID, "rootfs")
	if p.IsEnabled() && !force {
		return errors.Wrap(enabledError(p.Name()), "plugin already enabled")
	}
	spec, err := p.InitSpec(pm.config.ExecRoot)
	if err != nil {
		return err
	}

	c.restart = true
	c.exitChan = make(chan bool)

	pm.mu.Lock()
	pm.cMap[p] = c
	pm.mu.Unlock()

	var propRoot string
	if p.PropagatedMount != "" {
		propRoot = filepath.Join(filepath.Dir(p.Rootfs), "propagated-mount")

		if err = os.MkdirAll(propRoot, 0755); err != nil {
			logrus.Errorf("failed to create PropagatedMount directory at %s: %v", propRoot, err)
		}

		if err = mount.MakeRShared(propRoot); err != nil {
			return errors.Wrap(err, "error setting up propagated mount dir")
		}

		if err = mount.Mount(propRoot, p.PropagatedMount, "none", "rbind"); err != nil {
			return errors.Wrap(err, "error creating mount for propagated mount")
		}
	}

	rootFS := containerfs.NewLocalContainerFS(filepath.Join(pm.config.Root, p.PluginObj.ID, rootFSFileName))
	if err := initlayer.Setup(rootFS, idtools.IDPair{0, 0}); err != nil {
		return errors.WithStack(err)
	}

	stdout, stderr := makeLoggerStreams(p.GetID())
	if err := pm.executor.Create(p.GetID(), *spec, stdout, stderr); err != nil {
		if p.PropagatedMount != "" {
			if err := mount.Unmount(p.PropagatedMount); err != nil {
				logrus.Warnf("Could not unmount %s: %v", p.PropagatedMount, err)
			}
			if err := mount.Unmount(propRoot); err != nil {
				logrus.Warnf("Could not unmount %s: %v", propRoot, err)
			}
		}
	}

	return pm.pluginPostStart(p, c)
}

func (pm *Manager) pluginPostStart(p *v2.Plugin, c *controller) error {
	sockAddr := filepath.Join(pm.config.ExecRoot, p.GetID(), p.GetSocket())
	client, err := plugins.NewClientWithTimeout("unix://"+sockAddr, nil, time.Duration(c.timeoutInSecs)*time.Second)
	if err != nil {
		c.restart = false
		shutdownPlugin(p, c, pm.executor)
		return errors.WithStack(err)
	}

	p.SetPClient(client)

	// Initial sleep before net Dial to allow plugin to listen on socket.
	time.Sleep(500 * time.Millisecond)
	maxRetries := 3
	var retries int
	for {
		// net dial into the unix socket to see if someone's listening.
		conn, err := net.Dial("unix", sockAddr)
		if err == nil {
			conn.Close()
			break
		}

		time.Sleep(3 * time.Second)
		retries++

		if retries > maxRetries {
			logrus.Debugf("error net dialing plugin: %v", err)
			c.restart = false
			// While restoring plugins, we need to explicitly set the state to disabled
			pm.config.Store.SetState(p, false)
			shutdownPlugin(p, c, pm.executor)
			return err
		}

	}
	pm.config.Store.SetState(p, true)
	pm.config.Store.CallHandler(p)

	return pm.save(p)
}

func (pm *Manager) restore(p *v2.Plugin) error {
	stdout, stderr := makeLoggerStreams(p.GetID())
	if err := pm.executor.Restore(p.GetID(), stdout, stderr); err != nil {
		return err
	}

	if pm.config.LiveRestoreEnabled {
		c := &controller{}
		if isRunning, _ := pm.executor.IsRunning(p.GetID()); !isRunning {
			// plugin is not running, so follow normal startup procedure
			return pm.enable(p, c, true)
		}

		c.exitChan = make(chan bool)
		c.restart = true
		pm.mu.Lock()
		pm.cMap[p] = c
		pm.mu.Unlock()
		return pm.pluginPostStart(p, c)
	}

	return nil
}

func shutdownPlugin(p *v2.Plugin, c *controller, executor Executor) {
	pluginID := p.GetID()

	err := executor.Signal(pluginID, int(unix.SIGTERM))
	if err != nil {
		logrus.Errorf("Sending SIGTERM to plugin failed with error: %v", err)
	} else {
		select {
		case <-c.exitChan:
			logrus.Debug("Clean shutdown of plugin")
		case <-time.After(time.Second * 10):
			logrus.Debug("Force shutdown plugin")
			if err := executor.Signal(pluginID, int(unix.SIGKILL)); err != nil {
				logrus.Errorf("Sending SIGKILL to plugin failed with error: %v", err)
			}
			select {
			case <-c.exitChan:
				logrus.Debug("SIGKILL plugin shutdown")
			case <-time.After(time.Second * 10):
				logrus.Debug("Force shutdown plugin FAILED")
			}
		}
	}
}

func setupRoot(root string) error {
	if err := mount.MakePrivate(root); err != nil {
		return errors.Wrap(err, "error setting plugin manager root to private")
	}
	return nil
}

func (pm *Manager) disable(p *v2.Plugin, c *controller) error {
	if !p.IsEnabled() {
		return errors.Wrap(errDisabled(p.Name()), "plugin is already disabled")
	}

	c.restart = false
	shutdownPlugin(p, c, pm.executor)
	pm.config.Store.SetState(p, false)
	return pm.save(p)
}

// Shutdown stops all plugins and called during daemon shutdown.
func (pm *Manager) Shutdown() {
	plugins := pm.config.Store.GetAll()
	for _, p := range plugins {
		pm.mu.RLock()
		c := pm.cMap[p]
		pm.mu.RUnlock()

		if pm.config.LiveRestoreEnabled && p.IsEnabled() {
			logrus.Debug("Plugin active when liveRestore is set, skipping shutdown")
			continue
		}
		if pm.executor != nil && p.IsEnabled() {
			c.restart = false
			shutdownPlugin(p, c, pm.executor)
		}
	}
	mount.Unmount(pm.config.Root)
}

func (pm *Manager) upgradePlugin(p *v2.Plugin, configDigest digest.Digest, blobsums []digest.Digest, tmpRootFSDir string, privileges *types.PluginPrivileges) (err error) {
	config, err := pm.setupNewPlugin(configDigest, blobsums, privileges)
	if err != nil {
		return err
	}

	pdir := filepath.Join(pm.config.Root, p.PluginObj.ID)
	orig := filepath.Join(pdir, "rootfs")

	// Make sure nothing is mounted
	// This could happen if the plugin was disabled with `-f` with active mounts.
	// If there is anything in `orig` is still mounted, this should error out.
	if err := mount.RecursiveUnmount(orig); err != nil {
		return systemError{err}
	}

	backup := orig + "-old"
	if err := os.Rename(orig, backup); err != nil {
		return errors.Wrap(systemError{err}, "error backing up plugin data before upgrade")
	}

	defer func() {
		if err != nil {
			if rmErr := os.RemoveAll(orig); rmErr != nil && !os.IsNotExist(rmErr) {
				logrus.WithError(rmErr).WithField("dir", backup).Error("error cleaning up after failed upgrade")
				return
			}
			if mvErr := os.Rename(backup, orig); mvErr != nil {
				err = errors.Wrap(mvErr, "error restoring old plugin root on upgrade failure")
			}
			if rmErr := os.RemoveAll(tmpRootFSDir); rmErr != nil && !os.IsNotExist(rmErr) {
				logrus.WithError(rmErr).WithField("plugin", p.Name()).Errorf("error cleaning up plugin upgrade dir: %s", tmpRootFSDir)
			}
		} else {
			if rmErr := os.RemoveAll(backup); rmErr != nil && !os.IsNotExist(rmErr) {
				logrus.WithError(rmErr).WithField("dir", backup).Error("error cleaning up old plugin root after successful upgrade")
			}

			p.Config = configDigest
			p.Blobsums = blobsums
		}
	}()

	if err := os.Rename(tmpRootFSDir, orig); err != nil {
		return errors.Wrap(systemError{err}, "error upgrading")
	}

	p.PluginObj.Config = config
	err = pm.save(p)
	return errors.Wrap(err, "error saving upgraded plugin config")
}

func (pm *Manager) setupNewPlugin(configDigest digest.Digest, blobsums []digest.Digest, privileges *types.PluginPrivileges) (types.PluginConfig, error) {
	configRC, err := pm.blobStore.Get(configDigest)
	if err != nil {
		return types.PluginConfig{}, err
	}
	defer configRC.Close()

	var config types.PluginConfig
	dec := json.NewDecoder(configRC)
	if err := dec.Decode(&config); err != nil {
		return types.PluginConfig{}, errors.Wrapf(err, "failed to parse config")
	}
	if dec.More() {
		return types.PluginConfig{}, errors.New("invalid config json")
	}

	requiredPrivileges := computePrivileges(config)
	if err != nil {
		return types.PluginConfig{}, err
	}
	if privileges != nil {
		if err := validatePrivileges(requiredPrivileges, *privileges); err != nil {
			return types.PluginConfig{}, err
		}
	}

	return config, nil
}

// createPlugin creates a new plugin. take lock before calling.
func (pm *Manager) createPlugin(name string, configDigest digest.Digest, blobsums []digest.Digest, rootFSDir string, privileges *types.PluginPrivileges, opts ...CreateOpt) (p *v2.Plugin, err error) {
	if err := pm.config.Store.validateName(name); err != nil { // todo: this check is wrong. remove store
		return nil, validationError{err}
	}

	config, err := pm.setupNewPlugin(configDigest, blobsums, privileges)
	if err != nil {
		return nil, err
	}

	p = &v2.Plugin{
		PluginObj: types.Plugin{
			Name:   name,
			ID:     stringid.GenerateRandomID(),
			Config: config,
		},
		Config:   configDigest,
		Blobsums: blobsums,
	}
	p.InitEmptySettings()
	for _, o := range opts {
		o(p)
	}

	pdir := filepath.Join(pm.config.Root, p.PluginObj.ID)
	if err := os.MkdirAll(pdir, 0700); err != nil {
		return nil, errors.Wrapf(err, "failed to mkdir %v", pdir)
	}

	defer func() {
		if err != nil {
			os.RemoveAll(pdir)
		}
	}()

	if err := os.Rename(rootFSDir, filepath.Join(pdir, rootFSFileName)); err != nil {
		return nil, errors.Wrap(err, "failed to rename rootfs")
	}

	if err := pm.save(p); err != nil {
		return nil, err
	}

	pm.config.Store.Add(p) // todo: remove

	return p, nil
}
