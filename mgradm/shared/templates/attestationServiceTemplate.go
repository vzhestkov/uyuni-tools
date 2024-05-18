// SPDX-FileCopyrightText: 2024 SUSE LLC
//
// SPDX-License-Identifier: Apache-2.0

package templates

import (
	"io"
	"text/template"
)

const attestationServiceTemplate = `
# uyuni-server-attestation.service, generated by mgradm
# Use an uyuni-server-attestation.service.d/local.conf file to override
[Unit]
Description=Uyuni server attestation container service
Wants=network.target
After=network-online.target
[Service]
Environment=PODMAN_SYSTEMD_UNIT=%n
Restart=on-failure
ExecStartPre=/bin/rm -f %t/uyuni-server-attestation-%i.pid %t/%n.ctr-id
ExecStartPre=/usr/bin/podman rm --ignore --force -t 10 {{ .NamePrefix }}-server-attestation-%i
ExecStart=/usr/bin/podman run \
	--conmon-pidfile %t/uyuni-server-attestation-%i.pid \
	--cidfile=%t/%n-%i.ctr-id \
	--cgroups=no-conmon \
	--sdnotify=conmon \
	-d \
	-e database_connection  \
	-e database_user \
	-e database_password \
	--replace \
	--name {{ .NamePrefix }}-server-attestation-%i \
	--hostname {{ .NamePrefix }}-server-attestation-%i.mgr.internal \
	--network {{ .Network }} \
	${UYUNI_IMAGE}
ExecStop=/usr/bin/podman stop --ignore -t 10 --cidfile=%t/%n-%i.ctr-id
ExecStopPost=/usr/bin/podman rm -f --ignore -t 10 --cidfile=%t/%n-%i.ctr-id
PIDFile=%t/uyuni-server-attestation-%i.pid
TimeoutStopSec=60
TimeoutStartSec=60
Type=forking
[Install]
WantedBy=multi-user.target default.target
`

// PodmanServiceTemplateData POD information to create systemd file.
type AttestationServiceTemplateData struct {
	NamePrefix string
	Image      string
	Network    string
}

// Render will create the systemd configuration file.
func (data AttestationServiceTemplateData) Render(wr io.Writer) error {
	t := template.Must(template.New("service").Parse(attestationServiceTemplate))
	return t.Execute(wr, data)
}