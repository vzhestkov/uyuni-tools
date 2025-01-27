// SPDX-FileCopyrightText: 2024 SUSE LLC
//
// SPDX-License-Identifier: Apache-2.0

//go:build !nok8s

package kubernetes

import (
	"encoding/base64"
	"fmt"
	"os/exec"
	"path"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	migration_shared "github.com/uyuni-project/uyuni-tools/mgradm/cmd/migrate/shared"
	"github.com/uyuni-project/uyuni-tools/mgradm/shared/kubernetes"
	adm_utils "github.com/uyuni-project/uyuni-tools/mgradm/shared/utils"
	"github.com/uyuni-project/uyuni-tools/shared"
	shared_kubernetes "github.com/uyuni-project/uyuni-tools/shared/kubernetes"
	. "github.com/uyuni-project/uyuni-tools/shared/l10n"
	"github.com/uyuni-project/uyuni-tools/shared/ssl"
	"github.com/uyuni-project/uyuni-tools/shared/types"
	"github.com/uyuni-project/uyuni-tools/shared/utils"
)

func migrateToKubernetes(
	globalFlags *types.GlobalFlags,
	flags *kubernetesMigrateFlags,
	cmd *cobra.Command,
	args []string,
) error {
	for _, binary := range []string{"kubectl", "helm"} {
		if _, err := exec.LookPath(binary); err != nil {
			return fmt.Errorf(L("install %s before running this command"), binary)
		}
	}
	cnx := shared.NewConnection("kubectl", "", shared_kubernetes.ServerFilter)
	namespace, err := cnx.GetNamespace("")
	if err != nil {
		return utils.Errorf(err, L("failed retrieving namespace"))
	}

	serverImage, err := utils.ComputeImage(flags.Image.Registry, utils.DefaultTag, flags.Image)
	if err != nil {
		return utils.Errorf(err, L("failed to compute image URL"))
	}

	fqdn := args[0]
	if err := utils.IsValidFQDN(fqdn); err != nil {
		return err
	}

	// Find the SSH Socket and paths for the migration
	sshAuthSocket := migration_shared.GetSSHAuthSocket()
	sshConfigPath, sshKnownhostsPath := migration_shared.GetSSHPaths()

	// Prepare the migration script and folder
	scriptDir, cleaner, err := adm_utils.GenerateMigrationScript(fqdn, flags.User, true, flags.Prepare)
	if err != nil {
		return utils.Errorf(err, L("failed to generate migration script"))
	}

	defer cleaner()

	// We don't need the SSL certs at this point of the migration
	clusterInfos, err := shared_kubernetes.CheckCluster()
	if err != nil {
		return err
	}
	kubeconfig := clusterInfos.GetKubeconfig()

	// Install Uyuni with generated CA cert: an empty struct means no 3rd party cert
	var sslFlags adm_utils.InstallSSLFlags

	helmArgs := []string{}

	// Create a secret using SCC credentials if any are provided
	helmArgs, err = shared_kubernetes.AddSccSecret(helmArgs, flags.Helm.Uyuni.Namespace, &flags.Scc)
	if err != nil {
		return err
	}

	// Deploy for running migration command
	migrationArgs := append(helmArgs,
		"--set", "migration.ssh.agentSocket="+sshAuthSocket,
		"--set", "migration.ssh.configPath="+sshConfigPath,
		"--set", "migration.ssh.knownHostsPath="+sshKnownhostsPath,
		"--set", "migration.dataPath="+scriptDir,
	)

	if err := kubernetes.Deploy(cnx, flags.Image.Registry, &flags.Image, &flags.Helm, &sslFlags,
		clusterInfos, fqdn, false, flags.Prepare, migrationArgs...); err != nil {
		return utils.Errorf(err, L("cannot run deploy"))
	}

	// This is needed because folder with script needs to be mounted
	// check the node before scaling down
	nodeName, err := shared_kubernetes.GetNode(namespace, shared_kubernetes.ServerFilter)
	if err != nil {
		return utils.Errorf(err, L("cannot find node running uyuni"))
	}
	// Run the actual migration
	if err := adm_utils.RunMigration(cnx, scriptDir, "migrate.sh"); err != nil {
		return utils.Errorf(err, L("cannot run migration"))
	}

	extractedData, err := utils.ReadInspectData[utils.InspectResult](path.Join(scriptDir, "data"))
	if err != nil {
		return utils.Errorf(err, L("cannot read data from container"))
	}

	// After each command we want to scale to 0
	err = shared_kubernetes.ReplicasTo(namespace, shared_kubernetes.ServerApp, 0)
	if err != nil {
		return utils.Errorf(err, L("cannot set replicas to 0"))
	}

	if flags.Prepare {
		log.Info().Msg(L("Migration prepared. Run the 'migrate' command without '--prepare' to finish the migration."))
		return nil
	}

	defer func() {
		// if something is running, we don't need to set replicas to 1
		if _, err = shared_kubernetes.GetNode(namespace, shared_kubernetes.ServerFilter); err != nil {
			err = shared_kubernetes.ReplicasTo(namespace, shared_kubernetes.ServerApp, 1)
		}
	}()

	setupSslArray, err := setupSsl(&flags.Helm, kubeconfig, scriptDir, flags.Ssl.Password, flags.Image.PullPolicy)
	if err != nil {
		return utils.Errorf(err, L("cannot setup SSL"))
	}

	helmArgs = append(helmArgs,
		"--reset-values",
		"--set", "timezone="+extractedData.Timezone,
	)
	if flags.Mirror != "" {
		log.Warn().Msgf(L("The mirror data will not be migrated, ensure it is available at %s"), flags.Mirror)
		// TODO Handle claims for multi-node clusters
		helmArgs = append(helmArgs, "--set", "mirror.hostPath="+flags.Mirror)
	}
	helmArgs = append(helmArgs, setupSslArray...)

	// Run uyuni upgrade using the new ssl certificate
	if err = kubernetes.UyuniUpgrade(
		serverImage, flags.Image.PullPolicy, &flags.Helm, kubeconfig, fqdn, clusterInfos.Ingress, helmArgs...,
	); err != nil {
		return utils.Errorf(err, L("cannot upgrade helm chart to image %s using new SSL certificate"), serverImage)
	}

	if err := shared_kubernetes.WaitForDeployment(namespace, "uyuni", "uyuni"); err != nil {
		return utils.Errorf(err, L("cannot wait for deployment of %s"), serverImage)
	}

	err = shared_kubernetes.ReplicasTo(namespace, shared_kubernetes.ServerApp, 0)
	if err != nil {
		return utils.Errorf(err, L("cannot set replicas to 0"))
	}

	oldPgVersion := extractedData.CurrentPgVersion
	newPgVersion := extractedData.ImagePgVersion

	if oldPgVersion != newPgVersion {
		if err := kubernetes.RunPgsqlVersionUpgrade(flags.Image.Registry, flags.Image,
			flags.DBUpgradeImage, namespace, nodeName, oldPgVersion, newPgVersion,
		); err != nil {
			return utils.Errorf(err, L("cannot run PostgreSQL version upgrade script"))
		}
	}

	schemaUpdateRequired := oldPgVersion != newPgVersion
	if err := kubernetes.RunPgsqlFinalizeScript(
		serverImage, flags.Image.PullPolicy, namespace, nodeName, schemaUpdateRequired, true,
	); err != nil {
		return utils.Errorf(err, L("cannot run PostgreSQL finalisation script"))
	}

	if err := kubernetes.RunPostUpgradeScript(serverImage, flags.Image.PullPolicy, namespace, nodeName); err != nil {
		return utils.Errorf(err, L("cannot run post upgrade script"))
	}

	if err := kubernetes.UyuniUpgrade(
		serverImage, flags.Image.PullPolicy, &flags.Helm, kubeconfig, fqdn, clusterInfos.Ingress, helmArgs...,
	); err != nil {
		return utils.Errorf(err, L("cannot upgrade to image %s"), serverImage)
	}

	if err := shared_kubernetes.WaitForDeployment(namespace, "uyuni", "uyuni"); err != nil {
		return err
	}

	if err := cnx.CopyCaCertificate(fqdn); err != nil {
		return utils.Errorf(err, L("failed to add SSL CA certificate to host trusted certificates"))
	}
	return nil
}

// updateIssuer replaces the temporary SSL certificate issuer with the source server CA.
// Return additional helm args to use the SSL certificates.
func setupSsl(
	helm *adm_utils.HelmFlags,
	kubeconfig string,
	scriptDir string,
	password string,
	pullPolicy string) ([]string,
	error,
) {
	caCert := path.Join(scriptDir, "RHN-ORG-TRUSTED-SSL-CERT")
	caKey := path.Join(scriptDir, "RHN-ORG-PRIVATE-SSL-KEY")

	if utils.FileExists(caCert) && utils.FileExists(caKey) {
		key := base64.StdEncoding.EncodeToString(ssl.GetRsaKey(caKey, password))

		// Strip down the certificate text part
		out, err := utils.RunCmdOutput(zerolog.DebugLevel, "openssl", "x509", "-in", caCert)
		if err != nil {
			return []string{}, utils.Errorf(err, L("failed to strip text part from CA certificate"))
		}
		cert := base64.StdEncoding.EncodeToString(out)
		ca := types.SslPair{Cert: cert, Key: key}

		// An empty struct means no third party certificate
		sslFlags := adm_utils.InstallSSLFlags{}
		ret, err := kubernetes.DeployCertificate(helm, &sslFlags, cert, &ca, kubeconfig, "", pullPolicy)
		if err != nil {
			return []string{}, utils.Errorf(err, L("cannot deploy certificate"))
		}
		return ret, nil
	} else {
		// Handle third party certificates and CA
		sslFlags := adm_utils.InstallSSLFlags{
			Ca: types.CaChain{Root: caCert},
			Server: types.SslPair{
				Key:  path.Join(scriptDir, "spacewalk.key"),
				Cert: path.Join(scriptDir, "spacewalk.crt"),
			},
		}
		if err := kubernetes.DeployExistingCertificate(helm, &sslFlags, kubeconfig); err != nil {
			return []string{}, nil
		}
	}
	return []string{}, nil
}
