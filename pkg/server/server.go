package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	net2 "net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/rancher/k3s/pkg/daemons/config"
	"github.com/rancher/k3s/pkg/daemons/control"
	"github.com/rancher/k3s/pkg/deploy"
	"github.com/rancher/k3s/pkg/tls"
	v1 "github.com/rancher/k3s/types/apis/k3s.cattle.io/v1"
	"github.com/rancher/norman"
	"github.com/rancher/norman/pkg/clientaccess"
	"github.com/rancher/norman/pkg/dynamiclistener"
	"github.com/rancher/norman/pkg/resolvehome"
	"github.com/rancher/norman/types"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/net"
)

func resolveDataDir(dataDir string) (string, error) {
	if dataDir == "" {
		if os.Getuid() == 0 {
			dataDir = "/var/lib/rancher/k3s"
		} else {
			dataDir = "${HOME}/.rancher/k3s"
		}
	}

	dataDir = filepath.Join(dataDir, "server")
	return resolvehome.Resolve(dataDir)
}

func StartServer(ctx context.Context, config *Config) (string, error) {
	if err := setupDataDirAndChdir(&config.ControlConfig); err != nil {
		return "", err
	}

	if err := control.Server(ctx, &config.ControlConfig); err != nil {
		return "", errors.Wrap(err, "starting kubernetes")
	}

	certs, err := startNorman(ctx, &config.TLSConfig, &config.ControlConfig)
	if err != nil {
		return "", errors.Wrap(err, "starting tls server")
	}

	ip, err := net.ChooseHostInterface()
	if err != nil {
		ip = net2.ParseIP("127.0.0.1")
	}
	printTokens(certs, ip.String(), &config.TLSConfig, &config.ControlConfig)

	writeKubeConfig(certs, &config.TLSConfig, &config.ControlConfig)

	return certs, nil
}

func startNorman(ctx context.Context, tlsConfig *dynamiclistener.UserConfig, config *config.Control) (string, error) {
	var (
		err       error
		tlsServer dynamiclistener.ServerInterface
	)

	tlsConfig.Handler = router(config, config.Runtime.Tunnel, func() (string, error) {
		if tlsServer == nil {
			return "", nil
		}
		return tlsServer.CACert()
	})

	normanConfig := &norman.Config{
		Name:       "k3s",
		KubeConfig: config.Runtime.KubeConfigSystem,
		Clients: []norman.ClientFactory{
			v1.Factory,
		},
		Schemas: []*types.Schemas{
			v1.Schemas,
		},
		CRDs: map[*types.APIVersion][]string{
			&v1.APIVersion: {
				v1.ListenerConfigGroupVersionKind.Kind,
				v1.AddonGroupVersionKind.Kind,
			},
		},
		IgnoredKubeConfigEnv: true,
		GlobalSetup: func(ctx context.Context) (context.Context, error) {
			tlsServer, err = tls.NewServer(ctx, v1.ClientsFrom(ctx).ListenerConfig, *tlsConfig)
			return ctx, err
		},
		MasterControllers: []norman.ControllerRegister{
			func(ctx context.Context) error {
				dataDir := filepath.Join(config.DataDir, "manifests")
				if err := deploy.Stage(dataDir); err != nil {
					return err
				}
				if err := deploy.WatchFiles(ctx, config.Skips, dataDir); err != nil {
					return err
				}
				return nil
			},
		},
	}

	ctx, _, err = normanConfig.Build(ctx, nil)
	if err != nil {
		return "", err
	}

	for {
		certs, err := tlsServer.CACert()
		if err != nil {
			logrus.Infof("waiting to generate CA certs")
			time.Sleep(time.Second)
			continue
		}
		return certs, nil
	}
}

func HomeKubeConfig() (string, error) {
	return resolvehome.Resolve("${HOME}/.kube/k3s.yaml")
}

func printTokens(certs, advertiseIP string, tlsConfig *dynamiclistener.UserConfig, config *config.Control) {
	var (
		nodeFile string
	)

	if advertiseIP == "" {
		advertiseIP = "localhost"
	}

	if len(config.Runtime.NodeToken) > 0 {
		p := filepath.Join(config.DataDir, "node-token")
		if err := writeToken(config.Runtime.NodeToken, p, certs); err == nil {
			logrus.Infof("Node token is available at %s", p)
			nodeFile = p
		}
	}

	if len(nodeFile) > 0 {
		printToken(tlsConfig.HTTPSPort, advertiseIP, "To join node to cluster:", "agent")
	}

}

func writeKubeConfig(certs string, tlsConfig *dynamiclistener.UserConfig, config *config.Control) {
	clientToken := FormatToken(config.Runtime.ClientToken, certs)
	url := fmt.Sprintf("https://localhost:%d", tlsConfig.HTTPSPort)
	kubeConfig, err := HomeKubeConfig()
	def := true
	if err != nil {
		kubeConfig = filepath.Join(config.DataDir, "kubeconfig-k3s.yaml")
		def = false
	}

	if config.KubeConfigOutput != "" {
		kubeConfig = config.KubeConfigOutput
	}

	if err = clientaccess.AgentAccessInfoToKubeConfig(kubeConfig, url, clientToken); err != nil {
		logrus.Errorf("Failed to generate kubeconfig: %v", err)
	}

	if config.KubeConfigMode != "" {
		mode, err := strconv.ParseInt(config.KubeConfigMode, 8, 0)
		if err == nil {
			os.Chmod(kubeConfig, os.FileMode(mode))
		} else {
			logrus.Errorf("failed to set %s to mode %s: %v", kubeConfig, mode, err)
		}
	}

	logrus.Infof("Wrote kubeconfig %s", kubeConfig)
	if def {
		logrus.Infof("Run: %s kubectl", filepath.Base(os.Args[0]))
	}
}

func setupDataDirAndChdir(config *config.Control) error {
	var (
		err error
	)

	config.DataDir, err = resolveDataDir(config.DataDir)
	if err != nil {
		return err
	}

	dataDir := config.DataDir

	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return errors.Wrapf(err, "can not mkdir %s", dataDir)
	}

	if err := os.Chdir(dataDir); err != nil {
		return errors.Wrapf(err, "can not chdir %s", dataDir)
	}

	return nil
}

func readTokenFile(file string) (string, error) {
	content, err := ioutil.ReadFile(file)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(content)), nil
}

func printToken(httpsPort int, advertiseIP, prefix, cmd string) {
	ip := advertiseIP
	if ip == "" {
		hostIP, err := net.ChooseHostInterface()
		if err != nil {
			logrus.Error(err)
		}
		ip = hostIP.String()
	}

	logrus.Infof("%s k3s %s -s https://%s:%d -t ${NODE_TOKEN}", prefix, cmd, ip, httpsPort)
}

func FormatToken(token string, certs string) string {
	if len(token) == 0 {
		return token
	}

	prefix := "K10"
	if len(certs) > 0 {
		digest := sha256.Sum256([]byte(certs))
		prefix = "K10" + hex.EncodeToString(digest[:]) + "::"
	}

	return prefix + token
}

func writeToken(token, file, certs string) error {
	if len(token) == 0 {
		return nil
	}

	token = FormatToken(token, certs)
	return ioutil.WriteFile(file, []byte(token+"\n"), 0600)
}
