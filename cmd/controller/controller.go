/*
Copyright 2021 k0s authors

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
package controller

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"syscall"
	"time"

	"github.com/k0sproject/k0s/pkg/install"

	"github.com/avast/retry-go"
	"github.com/k0sproject/k0s/pkg/telemetry"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	workercmd "github.com/k0sproject/k0s/cmd/worker"
	"github.com/k0sproject/k0s/internal/pkg/dir"
	"github.com/k0sproject/k0s/internal/pkg/file"
	"github.com/k0sproject/k0s/internal/pkg/stringmap"
	"github.com/k0sproject/k0s/internal/pkg/stringslice"
	"github.com/k0sproject/k0s/pkg/apis/k0s.k0sproject.io/v1beta1"
	"github.com/k0sproject/k0s/pkg/applier"
	"github.com/k0sproject/k0s/pkg/build"
	"github.com/k0sproject/k0s/pkg/certificate"
	"github.com/k0sproject/k0s/pkg/component"
	"github.com/k0sproject/k0s/pkg/component/controller"
	"github.com/k0sproject/k0s/pkg/component/status"
	"github.com/k0sproject/k0s/pkg/config"
	"github.com/k0sproject/k0s/pkg/constant"
	"github.com/k0sproject/k0s/pkg/kubernetes"
	"github.com/k0sproject/k0s/pkg/performance"
	"github.com/k0sproject/k0s/pkg/token"
)

type CmdOpts config.CLIOptions

func NewControllerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "controller [join-token]",
		Short:   "Run controller",
		Aliases: []string{"server"},
		Example: `	Command to associate master nodes:
	CLI argument:
	$ k0s controller [join-token]

	or CLI flag:
	$ k0s controller --token-file [path_to_file]
	Note: Token can be passed either as a CLI argument or as a flag`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c := CmdOpts(config.GetCmdOpts())
			if len(args) > 0 {
				c.TokenArg = args[0]
			}
			if len(c.TokenArg) > 0 && len(c.TokenFile) > 0 {
				return fmt.Errorf("you can only pass one token argument either as a CLI argument 'k0s controller [join-token]' or as a flag 'k0s controller --token-file [path]'")
			}
			if len(c.DisableComponents) > 0 {
				for _, cmp := range c.DisableComponents {
					if !stringslice.Contains(config.AvailableComponents(), cmp) {
						return fmt.Errorf("unknown component %s", cmp)
					}
				}
			}
			if len(c.TokenFile) > 0 {
				bytes, err := os.ReadFile(c.TokenFile)
				if err != nil {
					return err
				}
				c.TokenArg = string(bytes)
			}
			if c.SingleNode {
				c.EnableWorker = true
				c.K0sVars.DefaultStorageType = "kine"
			}
			c.Logging = stringmap.Merge(c.CmdLogLevels, c.DefaultLogLevels)
			cmd.SilenceUsage = true
			return c.startController()
		},
	}

	// append flags
	cmd.Flags().AddFlagSet(config.GetPersistentFlagSet())
	cmd.PersistentFlags().AddFlagSet(config.GetControllerFlags())
	cmd.PersistentFlags().AddFlagSet(config.GetWorkerFlags())
	return cmd
}

func (c *CmdOpts) startController() error {
	c.NodeComponents = component.NewManager()
	c.ClusterComponents = component.NewManager()

	perfTimer := performance.NewTimer("controller-start").Buffer().Start()

	// create directories early with the proper permissions
	if err := dir.Init(c.K0sVars.DataDir, constant.DataDirMode); err != nil {
		return err
	}
	if err := dir.Init(c.K0sVars.CertRootDir, constant.CertRootDirMode); err != nil {
		return err
	}

	nodeConfig, err := config.GetNodeConfig(c.CfgFile, c.K0sVars)
	if err != nil {
		return err
	}
	c.NodeConfig = nodeConfig
	certificateManager := certificate.Manager{K0sVars: c.K0sVars}

	var joinClient *token.JoinClient
	if c.TokenArg != "" && c.needToJoin() {
		joinClient, err = joinController(c.TokenArg, c.K0sVars.CertRootDir)
		if err != nil {
			return fmt.Errorf("failed to join controller: %v", err)
		}
	}

	c.NodeComponents.AddSync(&controller.Certificates{
		ClusterSpec: c.NodeConfig.Spec,
		CertManager: certificateManager,
		K0sVars:     c.K0sVars,
	})

	logrus.Infof("using api address: %s", c.NodeConfig.Spec.API.Address)
	logrus.Infof("using listen port: %d", c.NodeConfig.Spec.API.Port)
	logrus.Infof("using sans: %s", c.NodeConfig.Spec.API.SANs)
	dnsAddress, err := c.NodeConfig.Spec.Network.DNSAddress()
	if err != nil {
		return err
	}
	logrus.Infof("DNS address: %s", dnsAddress)
	var storageBackend component.Component

	switch c.NodeConfig.Spec.Storage.Type {
	case v1beta1.KineStorageType:
		storageBackend = &controller.Kine{
			Config:  c.NodeConfig.Spec.Storage.Kine,
			K0sVars: c.K0sVars,
		}
	case v1beta1.EtcdStorageType:
		storageBackend = &controller.Etcd{
			CertManager: certificateManager,
			Config:      c.NodeConfig.Spec.Storage.Etcd,
			JoinClient:  joinClient,
			K0sVars:     c.K0sVars,
			LogLevel:    c.Logging["etcd"],
		}
	default:
		return fmt.Errorf("invalid storage type: %s", c.NodeConfig.Spec.Storage.Type)
	}
	logrus.Infof("using storage backend %s", c.NodeConfig.Spec.Storage.Type)
	c.NodeComponents.Add(storageBackend)

	// common factory to get the admin kube client that's needed in many components
	adminClientFactory := kubernetes.NewAdminClientFactory(c.K0sVars)
	c.NodeComponents.Add(&controller.APIServer{
		ClusterConfig:      c.NodeConfig,
		K0sVars:            c.K0sVars,
		LogLevel:           c.Logging["kube-apiserver"],
		Storage:            storageBackend,
		EnableKonnectivity: !c.SingleNode,
	})

	if c.NodeConfig.Spec.API.ExternalAddress != "" {
		c.NodeComponents.Add(&controller.K0sLease{
			ClusterConfig:     c.NodeConfig,
			KubeClientFactory: adminClientFactory,
		})
	}

	var leaderElector controller.LeaderElector

	// One leader elector per controller
	if c.NodeConfig.Spec.API.ExternalAddress != "" {
		leaderElector = controller.NewLeaderElector(adminClientFactory)
	} else {
		leaderElector = &controller.DummyLeaderElector{Leader: true}
	}
	c.NodeComponents.Add(leaderElector)

	c.NodeComponents.Add(&applier.Manager{
		K0sVars:           c.K0sVars,
		KubeClientFactory: adminClientFactory,
		LeaderElector:     leaderElector,
	})

	if !c.SingleNode && !stringslice.Contains(c.DisableComponents, constant.ControlAPIComponentName) {
		c.NodeComponents.Add(&controller.K0SControlAPI{
			ConfigPath: c.CfgFile,
			K0sVars:    c.K0sVars,
		})
	}

	if c.NodeConfig.Spec.API.ExternalAddress != "" {
		c.NodeComponents.Add(controller.NewEndpointReconciler(
			c.NodeConfig,
			leaderElector,
			adminClientFactory,
		))
	}

	if !stringslice.Contains(c.DisableComponents, constant.CsrApproverComponentName) {
		c.NodeComponents.Add(controller.NewCSRApprover(c.NodeConfig,
			leaderElector,
			adminClientFactory))
	}

	if c.EnableK0sCloudProvider {
		c.NodeComponents.Add(
			controller.NewK0sCloudProvider(
				c.K0sVars.AdminKubeConfigPath,
				c.K0sCloudProviderUpdateFrequency,
				c.K0sCloudProviderPort,
			),
		)
	}

	workload := false
	if c.SingleNode || c.EnableWorker {
		workload = true
	}

	c.NodeComponents.Add(&status.Status{
		StatusInformation: install.K0sStatus{
			Pid:           os.Getpid(),
			Role:          "controller",
			Args:          os.Args,
			Version:       build.Version,
			Workloads:     workload,
			K0sVars:       c.K0sVars,
			ClusterConfig: c.NodeConfig,
		},
		Socket: config.StatusSocket,
	})

	perfTimer.Checkpoint("starting-component-init")
	// init Node components
	if err := c.NodeComponents.Init(); err != nil {
		return err
	}
	perfTimer.Checkpoint("finished-node-component-init")

	// Set up signal handling. Use buffered channel so we dont miss
	// signals during startup
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer func() {
		signal.Stop(ch)
		cancel()
	}()

	go func() {
		select {
		case <-ch:
			logrus.Info("Shutting down k0s controller")
			cancel()
		case <-ctx.Done():
			logrus.Debug("Context done in go-routine")
		}
	}()

	perfTimer.Checkpoint("starting-node-components")

	// Start components
	err = c.NodeComponents.Start(ctx)
	perfTimer.Checkpoint("finished-starting-node-components")
	if err != nil {
		logrus.Errorf("failed to start controller node components: %v", err)
		ch <- syscall.SIGTERM
	}

	// start Bootstrapping Reconcilers
	err = c.startBootstrapReconcilers(adminClientFactory, leaderElector)
	if err != nil {
		logrus.Errorf("failed to start bootstrapping reconcilers: %v", err)
		ch <- syscall.SIGTERM
	}

	// in-cluster component reconcilers
	reconcilers, err := c.createClusterReconcilers(ctx, adminClientFactory)
	if err != nil {
		logrus.Errorf("failed to start reconcilers: %v", err)
		ch <- syscall.SIGTERM
	}

	c.Reconcilers = reconcilers
	perfTimer.Checkpoint("starting-reconcilers")

	// Start all reconcilers
	err = c.startClusterReconcilers(ctx, reconcilers)
	if err != nil {
		logrus.Errorf("failed to start cluster reconcilers: %v", err)
		ch <- syscall.SIGTERM
	}
	perfTimer.Checkpoint("started-reconcilers")

	existingCNI := c.existingCNIProvider()
	if existingCNI != "" && existingCNI != c.ClusterConfig.Spec.Network.Provider {
		logrus.Errorf("cannot change CNI from %v to %v", existingCNI, c.ClusterConfig.Spec.Network.Provider)
	}

	if !c.SingleNode && !stringslice.Contains(c.DisableComponents, constant.KonnectivityServerComponentName) {
		c.ClusterComponents.Add(&controller.Konnectivity{
			ClusterConfig:     c.ClusterConfig,
			LogLevel:          c.Logging[constant.KonnectivityServerComponentName],
			K0sVars:           c.K0sVars,
			KubeClientFactory: adminClientFactory,
		})
	}
	if !stringslice.Contains(c.DisableComponents, constant.KubeSchedulerComponentName) {
		c.ClusterComponents.Add(&controller.Scheduler{
			ClusterConfig: c.ClusterConfig,
			LogLevel:      c.Logging[constant.KubeSchedulerComponentName],
			K0sVars:       c.K0sVars,
		})
	}
	if !stringslice.Contains(c.DisableComponents, constant.KubeControllerManagerComponentName) {
		c.ClusterComponents.Add(&controller.Manager{
			ClusterConfig: c.ClusterConfig,
			LogLevel:      c.Logging[constant.KubeControllerManagerComponentName],
			K0sVars:       c.K0sVars,
		})

		if c.ClusterConfig.Spec.Telemetry.Enabled {
			c.ClusterComponents.Add(&telemetry.Component{
				ClusterConfig:     c.ClusterConfig,
				Version:           build.Version,
				K0sVars:           c.K0sVars,
				KubeClientFactory: adminClientFactory,
			})
		}
	}

	perfTimer.Checkpoint("starting-cluster-components-init")
	// init Cluster components
	if err := c.ClusterComponents.Init(); err != nil {
		return err
	}
	perfTimer.Checkpoint("finished cluster-component-init")

	err = c.startClusterComponents(ctx)
	if err != nil {
		logrus.Errorf("failed to start cluster components: %s", err)
		ch <- syscall.SIGTERM
	}
	perfTimer.Checkpoint("finished-starting-cluster-components")

	if err == nil && c.EnableWorker {
		perfTimer.Checkpoint("starting-worker")

		err = c.startControllerWorker(ctx, c.WorkerProfile)
		if err != nil {
			logrus.Errorf("failed to start worker components: %s", err)
			if err := c.ClusterComponents.Stop(); err != nil {
				logrus.Errorf("ClusterComponents.Stop: %s", err)
			}
			if err := c.NodeComponents.Stop(); err != nil {
				logrus.Errorf("NodeCompnents.Stop: %s", err)
			}
			return err
		}
	}
	perfTimer.Checkpoint("started-worker")

	perfTimer.Output()

	// Wait for k0s process termination
	<-ctx.Done()
	logrus.Debug("Context done in main")

	perfTimer.Output()

	// Stop all reconcilers first
	for _, reconciler := range c.Reconcilers {
		if err := reconciler.Stop(); err != nil {
			logrus.Warningf("failed to stop reconciler: %s", err.Error())
		}
	}

	// Stop components
	if err := c.ClusterComponents.Stop(); err != nil {
		logrus.Errorf("error while stopping node component %s", err)
	}

	// Stop components
	if err := c.NodeComponents.Stop(); err != nil {
		logrus.Errorf("error while stopping node component %s", err)
	}

	return nil
}

func (c *CmdOpts) startClusterComponents(ctx context.Context) error {
	errs, ctx := errgroup.WithContext(ctx)

	// number of components to start
	num := len(c.ClusterComponents.Components)

	for i := 0; i < num; i++ {
		errs.Go(func() error {
			err := c.ClusterComponents.Start(ctx)
			return fmt.Errorf("error starting cluster components, bailing", err)
		})
	}

	// Wait for completion and return the first error (if any)
	return errs.Wait()
}

func (c *CmdOpts) startClusterReconcilers(ctx context.Context, reconcilers map[string]component.Component) error {
	errs, ctx := errgroup.WithContext(ctx)

	// Start all reconcilers
	for name, reconciler := range reconcilers {
		logrus.Infof("running reconciler: %s", name)
		errs.Go(func() error {
			err := reconciler.Run()
			return fmt.Errorf("failed to start reconciler: %s", err.Error())
		})
	}

	// Wait for completion and return the first error (if any)
	return errs.Wait()
}

func (c *CmdOpts) startBootstrapReconcilers(cf kubernetes.ClientFactoryInterface, leaderElector controller.LeaderElector) error {
	reconcilers := make(map[string]component.Component)

	if !stringslice.Contains(c.DisableComponents, constant.HelmComponentName) {
		logrus.Debug("starting manifest saver")
		manifestsSaver, err := controller.NewManifestsSaver("helm", c.K0sVars.DataDir)
		if err != nil {
			logrus.Warnf("failed to initialize reconcilers manifests saver: %s", err.Error())
			return err
		}
		reconcilers["helmCrd"] = controller.NewCRD(manifestsSaver)
		reconcilers["helmAddons"] = controller.NewHelmAddons(manifestsSaver, c.K0sVars, cf, leaderElector)
	}

	if !stringslice.Contains(c.DisableComponents, constant.APIConfigComponentName) {
		logrus.Debug("starting api-config reconciler")

		cfgReconciler, err := controller.NewClusterConfigReconciler(c.CfgFile, leaderElector, c.K0sVars, c.ClusterComponents)
		if err != nil {
			logrus.Warnf("failed to initialize cluster-config reconciler: %s", err.Error())
			return err
		}
		reconcilers["clusterConfig"] = cfgReconciler
	}

	// Start all reconcilers
	for name, reconciler := range reconcilers {
		logrus.Infof("running bootstrap reconciler: %s", name)
		err := reconciler.Run()
		if err != nil {
			return fmt.Errorf("failed to start reconciler: %s", err.Error())
		}
	}

	clusterConfig, err := config.GetConfigFromAPI(c.K0sVars.AdminKubeConfigPath)
	if err != nil {
		return err
	}
	c.ClusterConfig = clusterConfig

	return nil
}

func (c *CmdOpts) createClusterReconcilers(ctx context.Context, cf kubernetes.ClientFactoryInterface) (map[string]component.Component, error) {
	var err error
	errs, ctx := errgroup.WithContext(ctx)

	reconcilers := make(map[string]component.Component)

	if !stringslice.Contains(c.DisableComponents, constant.DefaultPspComponentName) {
		errs.Go(func() error {
			defaultPSP, err := controller.NewDefaultPSP(c.K0sVars)
			if err != nil {
				logrus.Warnf("failed to initialize default PSP reconciler: %s", err.Error())
			} else {
				reconcilers["default-psp"] = defaultPSP
			}
			return nil
		})
	}

	if !stringslice.Contains(c.DisableComponents, constant.KubeProxyComponentName) {
		errs.Go(func() error {
			proxy, err := controller.NewKubeProxy(c.CfgFile, c.K0sVars)
			if err != nil {
				logrus.Warnf("failed to initialize kube-proxy reconciler: %s", err.Error())
			} else {
				reconcilers["kube-proxy"] = proxy
			}
			return nil
		})
	}

	if !stringslice.Contains(c.DisableComponents, constant.CoreDNSComponentname) {
		errs.Go(func() error {
			coreDNS, err := controller.NewCoreDNS(c.K0sVars, cf)
			if err != nil {
				logrus.Warnf("failed to initialize CoreDNS reconciler: %s", err.Error())
			} else {
				reconcilers["coredns"] = coreDNS
			}
			return nil
		})
	}

	if !stringslice.Contains(c.DisableComponents, constant.NetworkProviderComponentName) {
		errs.Go(func() error {
			logrus.Infof("initializing network reconciler for provider %s", c.ClusterConfig.Spec.Network.Provider)
			switch c.ClusterConfig.Spec.Network.Provider {
			case "custom":
				logrus.Warnf("network provider set to custom, k0s will not manage it")
			case "calico":
				err = c.initCalico(reconcilers)
			case "kuberouter":
				err = c.initKubeRouter(reconcilers)
				if err != nil {
					logrus.Warnf("failed to initialize network reconciler: %s", err.Error())
					return err
				}
			}
			return nil
		})
		return reconcilers, errs.Wait()
	}

	if !stringslice.Contains(c.DisableComponents, constant.MetricsServerComponentName) {
		errs.Go(func() error {
			metricServer, err := controller.NewMetricServer(c.K0sVars, cf)
			if err != nil {
				logrus.Warnf("failed to initialize metrics-server reconciler: %s", err.Error())
				return err
			}
			reconcilers["metricServer"] = metricServer
			return nil
		})
		return reconcilers, errs.Wait()
	}

	if !stringslice.Contains(c.DisableComponents, constant.KubeletConfigComponentName) {
		errs.Go(func() error {
			kubeletConfig, err := controller.NewKubeletConfig(c.K0sVars)
			if err != nil {
				logrus.Warnf("failed to initialize kubelet config reconciler: %s", err.Error())
				return err
			}
			reconcilers["kubeletConfig"] = kubeletConfig
			return nil
		})
		return reconcilers, errs.Wait()
	}

	if !stringslice.Contains(c.DisableComponents, constant.SystemRbacComponentName) {
		errs.Go(func() error {
			systemRBAC, err := controller.NewSystemRBAC(c.K0sVars.ManifestsDir)
			if err != nil {
				logrus.Warnf("failed to initialize system RBAC reconciler: %s", err.Error())
				return err
			}
			reconcilers["systemRBAC"] = systemRBAC
			return nil
		})
		return reconcilers, errs.Wait()
	}
	return reconcilers, nil
}

func (c *CmdOpts) initCalico(reconcilers map[string]component.Component) error {
	calicoSaver, err := controller.NewManifestsSaver("calico", c.K0sVars.DataDir)
	if err != nil {
		logrus.Warnf("failed to initialize reconcilers manifests saver: %s", err.Error())
		return err
	}
	calicoInitSaver, err := controller.NewManifestsSaver("calico_init", c.K0sVars.DataDir)
	if err != nil {
		logrus.Warnf("failed to initialize reconcilers manifests saver: %s", err.Error())
		return err
	}
	calico, err := controller.NewCalico(c.ClusterConfig, calicoInitSaver, calicoSaver)
	if err != nil {
		logrus.Warnf("failed to initialize calico reconciler: %s", err.Error())
		return err
	}
	reconcilers["calico"] = calico

	return nil
}

func (c *CmdOpts) initKubeRouter(reconcilers map[string]component.Component) error {
	mfSaver, err := controller.NewManifestsSaver("kuberouter", c.K0sVars.DataDir)
	if err != nil {
		logrus.Warnf("failed to initialize kube-router manifests saver: %s", err.Error())
		return err
	}
	kubeRouter, err := controller.NewKubeRouter(c.ClusterConfig, mfSaver)
	if err != nil {
		logrus.Warnf("failed to initialize kube-router reconciler: %s", err.Error())
		return err
	}
	reconcilers["kube-router"] = kubeRouter

	return nil
}

func (c *CmdOpts) existingCNIProvider() string {
	calicoManifestPath := path.Join(c.K0sVars.ManifestsDir, "calico", "calico-DaemonSet-calico-node.yaml")
	if file.Exists(calicoManifestPath) {
		return "calico"
	}

	kubeRouterManifestPath := path.Join(c.K0sVars.ManifestsDir, "kuberouter", "kube-router.yaml")
	if file.Exists(kubeRouterManifestPath) {
		return "kuberouter"
	}

	return ""
}

func (c *CmdOpts) startControllerWorker(_ context.Context, profile string) error {
	var bootstrapConfig string
	if !file.Exists(c.K0sVars.KubeletAuthConfigPath) {
		// wait for controller to start up
		err := retry.Do(func() error {
			if !file.Exists(c.K0sVars.AdminKubeConfigPath) {
				return fmt.Errorf("file does not exist: %s", c.K0sVars.AdminKubeConfigPath)
			}
			return nil
		})
		if err != nil {
			return err
		}

		err = retry.Do(func() error {
			// five minutes here are coming from maximum theoretical duration of kubelet bootstrap process
			// we use retry.Do with 10 attempts, back-off delay and delay duration 500 ms which gives us
			// 225 seconds here
			tokenAge := time.Second * 225
			cfg, err := token.CreateKubeletBootstrapConfig(c.NodeConfig, c.K0sVars, "worker", tokenAge)
			if err != nil {
				return err
			}
			bootstrapConfig = cfg
			return nil
		})
		if err != nil {
			return err
		}
	}
	// cast and make a copy of the controller cmdOpts
	// so we can use the same opts to start the worker
	// Needs to be a copy so we don't mess up the original
	// token and possibly other args
	workerCmdOpts := *(*workercmd.CmdOpts)(c)
	workerCmdOpts.TokenArg = bootstrapConfig
	workerCmdOpts.WorkerProfile = profile
	return workerCmdOpts.StartWorker()
}

// If we've got CA in place we assume the node has already joined previously
func (c *CmdOpts) needToJoin() bool {
	if file.Exists(filepath.Join(c.K0sVars.CertRootDir, "ca.key")) &&
		file.Exists(filepath.Join(c.K0sVars.CertRootDir, "ca.crt")) {
		return false
	}
	return true
}

func writeCerts(caData v1beta1.CaResponse, certRootDir string) error {
	type fileData struct {
		path string
		data []byte
		mode fs.FileMode
	}
	for _, f := range []fileData{
		{path: filepath.Join(certRootDir, "ca.key"), data: caData.Key, mode: constant.CertSecureMode},
		{path: filepath.Join(certRootDir, "ca.crt"), data: caData.Cert, mode: constant.CertMode},
		{path: filepath.Join(certRootDir, "sa.key"), data: caData.SAKey, mode: constant.CertSecureMode},
		{path: filepath.Join(certRootDir, "sa.pub"), data: caData.SAPub, mode: constant.CertMode},
	} {
		err := os.WriteFile(f.path, f.data, f.mode)
		if err != nil {
			return fmt.Errorf("failed to write %s: %w", f.path, err)
		}
	}
	return nil
}

func joinController(tokenArg string, certRootDir string) (*token.JoinClient, error) {
	joinClient, err := token.JoinClientFromToken(tokenArg)
	if err != nil {
		return nil, fmt.Errorf("failed to create join client: %w", err)
	}

	var caData v1beta1.CaResponse
	err = retry.Do(func() error {
		caData, err = joinClient.GetCA()
		if err != nil {
			return fmt.Errorf("failed to sync CA: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return joinClient, writeCerts(caData, certRootDir)
}
