// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package operatingsystemconfig

import (
	"context"
	_ "embed"
	"fmt"
	"path/filepath"

	"github.com/gardener/gardener/extensions/pkg/controller/operatingsystemconfig"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	"github.com/go-logr/logr"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/gardener/gardener-extension-os-gardenlinux/pkg/gardenlinux"
	"github.com/gardener/gardener-extension-os-gardenlinux/pkg/memoryone"
)

type actuator struct {
	client client.Client
}

// NewActuator creates a new Actuator that updates the status of the handled OperatingSystemConfig resources.
func NewActuator(mgr manager.Manager) operatingsystemconfig.Actuator {
	return &actuator{
		client: mgr.GetClient(),
	}
}

func (a *actuator) Reconcile(ctx context.Context, log logr.Logger, osc *extensionsv1alpha1.OperatingSystemConfig) ([]byte, []extensionsv1alpha1.Unit, []extensionsv1alpha1.File, *extensionsv1alpha1.InPlaceUpdateConfig, error) {
	switch purpose := osc.Spec.Purpose; purpose {
	case extensionsv1alpha1.OperatingSystemConfigPurposeProvision:
		userData, err := a.handleProvisionOSC(ctx, osc)
		return []byte(userData), nil, nil, nil, err

	case extensionsv1alpha1.OperatingSystemConfigPurposeReconcile:
		extensionUnits, extensionFiles, inPlaceUpdateConfig, err := a.handleReconcileOSC(osc)
		return nil, extensionUnits, extensionFiles, inPlaceUpdateConfig, err

	default:
		return nil, nil, nil, nil, fmt.Errorf("unknown purpose: %s", purpose)
	}
}

func (a *actuator) Delete(_ context.Context, _ logr.Logger, _ *extensionsv1alpha1.OperatingSystemConfig) error {
	return nil
}

func (a *actuator) Migrate(ctx context.Context, log logr.Logger, osc *extensionsv1alpha1.OperatingSystemConfig) error {
	return a.Delete(ctx, log, osc)
}

func (a *actuator) ForceDelete(ctx context.Context, log logr.Logger, osc *extensionsv1alpha1.OperatingSystemConfig) error {
	return a.Delete(ctx, log, osc)
}

func (a *actuator) Restore(ctx context.Context, log logr.Logger, osc *extensionsv1alpha1.OperatingSystemConfig) ([]byte, []extensionsv1alpha1.Unit, []extensionsv1alpha1.File, *extensionsv1alpha1.InPlaceUpdateConfig, error) {
	return a.Reconcile(ctx, log, osc)
}

func (a *actuator) handleProvisionOSC(ctx context.Context, osc *extensionsv1alpha1.OperatingSystemConfig) (string, error) {
	writeFilesToDiskScript, err := operatingsystemconfig.FilesToDiskScript(ctx, a.client, osc.Namespace, osc.Spec.Files)
	if err != nil {
		return "", err
	}
	writeUnitsToDiskScript := operatingsystemconfig.UnitsToDiskScript(osc.Spec.Units)

	script := `#!/bin/bash
if [ ! -s /etc/containerd/config.toml ]; then
  mkdir -p /etc/containerd/
  containerd config default > /etc/containerd/config.toml
  chmod 0644 /etc/containerd/config.toml
fi

mkdir -p /etc/systemd/system/containerd.service.d
cat <<EOF > /etc/systemd/system/containerd.service.d/11-exec_config.conf
[Service]
ExecStart=
ExecStart=/usr/bin/containerd --config=/etc/containerd/config.toml
EOF
chmod 0644 /etc/systemd/system/containerd.service.d/11-exec_config.conf
` + writeFilesToDiskScript + `
` + writeUnitsToDiskScript + `
grep -sq "^nfsd$" /etc/modules || echo "nfsd" >>/etc/modules
modprobe nfsd
nslookup $(hostname) || systemctl restart systemd-networkd

systemctl daemon-reload
systemctl enable containerd && systemctl restart containerd
systemctl enable docker && systemctl restart docker
`
	for _, unit := range osc.Spec.Units {
		script += fmt.Sprintf(`systemctl enable '%s' && systemctl restart --no-block '%s'
`, unit.Name, unit.Name)
	}

	if osc.Spec.Type == memoryone.OSTypeMemoryOneGardenLinux {
		return wrapIntoMemoryOneHeaderAndFooter(osc, script)
	}

	return script, nil
}

func wrapIntoMemoryOneHeaderAndFooter(osc *extensionsv1alpha1.OperatingSystemConfig, in string) (string, error) {
	config, err := memoryone.Configuration(osc)
	if err != nil {
		return "", err
	}

	out := `Content-Type: multipart/mixed; boundary="==BOUNDARY=="
MIME-Version: 1.0
--==BOUNDARY==
Content-Type: text/x-vsmp; section=vsmp`

	if config != nil && config.SystemMemory != nil {
		out += fmt.Sprintf(`
system_memory=%s`, *config.SystemMemory)
	}
	if config != nil && config.MemoryTopology != nil {
		out += fmt.Sprintf(`
mem_topology=%s`, *config.MemoryTopology)
	}

	out += `
--==BOUNDARY==
Content-Type: text/x-shellscript
` + in + `
--==BOUNDARY==`

	return out, nil
}

var (
	scriptContentInPlaceUpdate          []byte
	scriptContentGFunctions             []byte
	scriptContentKubeletCGroupDriver    []byte
	scriptContentContainerdCGroupDriver []byte
)

func init() {
	var err error

	scriptContentInPlaceUpdate, err = gardenlinux.Templates.ReadFile(filepath.Join("scripts", "inplace-update.sh"))
	utilruntime.Must(err)
	scriptContentGFunctions, err = gardenlinux.Templates.ReadFile(filepath.Join("scripts", "g_functions.sh"))
	utilruntime.Must(err)
	scriptContentKubeletCGroupDriver, err = gardenlinux.Templates.ReadFile(filepath.Join("scripts", "kubelet_cgroup_driver.sh"))
	utilruntime.Must(err)
	scriptContentContainerdCGroupDriver, err = gardenlinux.Templates.ReadFile(filepath.Join("scripts", "containerd_cgroup_driver.sh"))
	utilruntime.Must(err)
}

func (a *actuator) handleReconcileOSC(osConfig *extensionsv1alpha1.OperatingSystemConfig) ([]extensionsv1alpha1.Unit, []extensionsv1alpha1.File, *extensionsv1alpha1.InPlaceUpdateConfig, error) {
	var (
		extensionUnits []extensionsv1alpha1.Unit
		extensionFiles []extensionsv1alpha1.File
	)

	filePathOSUpdateScript := filepath.Join(gardenlinux.ScriptLocation, "inplace-update.sh")
	extensionFiles = append(extensionFiles, extensionsv1alpha1.File{
		Path:        filePathOSUpdateScript,
		Content:     extensionsv1alpha1.FileContent{Inline: &extensionsv1alpha1.FileContentInline{Data: string(scriptContentInPlaceUpdate)}},
		Permissions: &gardenlinux.ScriptPermissions,
	})
	inPlaceUpdateConfig := &extensionsv1alpha1.InPlaceUpdateConfig{
		OSUpdateCommand:     ptr.To(filePathOSUpdateScript),
		OSUpdateCommandArgs: []string{ptr.Deref(osConfig.Spec.OSVersion, "")},
	}

	filePathFunctionsHelperScript := filepath.Join(gardenlinux.ScriptLocation, "g_functions.sh")
	extensionFiles = append(extensionFiles, extensionsv1alpha1.File{
		Path:        filePathFunctionsHelperScript,
		Content:     extensionsv1alpha1.FileContent{Inline: &extensionsv1alpha1.FileContentInline{Data: string(scriptContentGFunctions)}},
		Permissions: &gardenlinux.ScriptPermissions,
	})

	// add scripts and dropins for kubelet
	filePathKubeletCGroupDriverScript := filepath.Join(gardenlinux.ScriptLocation, "kubelet_cgroup_driver.sh")
	extensionFiles = append(extensionFiles, extensionsv1alpha1.File{
		Path:        filePathKubeletCGroupDriverScript,
		Content:     extensionsv1alpha1.FileContent{Inline: &extensionsv1alpha1.FileContentInline{Data: string(scriptContentKubeletCGroupDriver)}},
		Permissions: &gardenlinux.ScriptPermissions,
	})
	extensionUnits = append(extensionUnits, extensionsv1alpha1.Unit{
		Name: "kubelet.service",
		DropIns: []extensionsv1alpha1.DropIn{{
			Name: "10-configure-cgroup-driver.conf",
			Content: `[Service]
ExecStartPre=` + filePathKubeletCGroupDriverScript + `
`,
		}},
		FilePaths: []string{filePathFunctionsHelperScript, filePathKubeletCGroupDriverScript},
	})

	// add scripts and dropins for containerd
	filePathContainerdCGroupDriverScript := filepath.Join(gardenlinux.ScriptLocation, "containerd_cgroup_driver.sh")
	extensionFiles = append(extensionFiles, extensionsv1alpha1.File{
		Path:        filePathContainerdCGroupDriverScript,
		Content:     extensionsv1alpha1.FileContent{Inline: &extensionsv1alpha1.FileContentInline{Data: string(scriptContentContainerdCGroupDriver)}},
		Permissions: &gardenlinux.ScriptPermissions,
	})
	extensionUnits = append(extensionUnits, extensionsv1alpha1.Unit{
		Name: "containerd.service",
		DropIns: []extensionsv1alpha1.DropIn{{
			Name: "10-configure-cgroup-driver.conf",
			Content: `[Service]
ExecStartPre=` + filePathContainerdCGroupDriverScript + `
`,
		}},
		FilePaths: []string{filePathFunctionsHelperScript, filePathContainerdCGroupDriverScript},
	})

	return extensionUnits, extensionFiles, inPlaceUpdateConfig, nil
}
