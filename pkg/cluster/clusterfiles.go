package cluster

import (
	"bytes"
	"text/template"
)

func ExecuteTemplate(tmplStr string, tmplData interface{}) (bytes.Buffer, error) {
	var out bytes.Buffer
	tmpl, err := template.New("").Parse(tmplStr)
	if err != nil {
		return out, err
	}
	if err := tmpl.Execute(&out, tmplData); err != nil {
		return out, err
	}
	return out, nil
}

const DockerDaemonConfig = `{
    "insecure-registries": ["10.22.0.1:5000"],
    "default-runtime": "custom",
    "runtimes": {
        "custom": { "path": "/usr/bin/kube-spawn-runc" }
    },
    "storage-driver": "overlay2"
}
`

const DockerSystemdDropin = `[Service]
Environment="DOCKER_OPTS=--exec-opt native.cgroupdriver=cgroupfs"
`

const RktletSystemdUnitTmpl = `[Unit]
Description=rktlet: The rkt implementation of a Kubernetes Container Runtime
Documentation=https://github.com/kubernetes-incubator/rktlet/tree/master/docs

[Service]
ExecStart=/usr/bin/rktlet --net={{ .CNIPlugin }}
Restart=always
StartLimitInterval=0
RestartSec=10

[Install]
WantedBy=multi-user.target
`

// https://github.com/kinvolk/kube-spawn/issues/99
// https://github.com/weaveworks/weave/issues/2601
const WeaveSystemdNetworkdConfig = `[Match]
Name=weave datapath vethwe*

[Link]
Unmanaged=yes
`

const KubespawnBootstrapScriptTmpl = `#!/bin/bash

set -euxo pipefail

echo "root:root" | chpasswd
echo "core:core" | chpasswd

systemctl enable kubelet.service
systemctl enable sshd.service

{{ if eq .ContainerRuntime "docker" -}}systemctl start --no-block docker.service{{- end}}
{{ if eq .ContainerRuntime "rkt" -}}systemctl start --no-block rktlet.service
mkdir -p /usr/lib/rkt/plugins
ln -s /opt/cni/bin/ /usr/lib/rkt/plugins/net
ln -sfT /etc/cni/net.d /etc/rkt/net.d{{- end}}

mkdir -p /var/lib/weave

# necessary to prevent docker from being blocked
systemctl mask systemd-networkd-wait-online.service

[ -f /etc/kubernetes/kubelet.conf ] && kubeadm reset {{.KubeadmResetOptions}}
systemctl start --no-block kubelet.service
`

// --fail-swap-on=false is necessary for k8s 1.8 or newer.

// For rktlet, --container-runtime must be "remote", not "rkt".
// --container-runtime-endpoint needs to point to the unix socket,
// which rktlet listens on.

// --cgroups-per-qos should be set to false, so that we can avoid issues with
// different formats of cgroup paths between k8s and systemd.

// --enforce-node-allocatable= and --eviction-hard= are set to make the
// kubelet *not* evicting pods from the system, even when memory or disk
// space is low. Otherwise `kube-spawn start` could fail because the
// k8s containers started by kubeadm get evicted immediately.
const KubeletSystemdDropinTmpl = `[Service]
Environment="KUBELET_CGROUP_ARGS=--cgroup-driver={{ if .UseLegacyCgroupDriver }}cgroupfs{{else}}systemd{{end}}"
Environment="KUBELET_EXTRA_ARGS=\
{{ if ne .ContainerRuntime "docker" -}}--container-runtime=remote \
--container-runtime-endpoint={{.RuntimeEndpoint}} \
--runtime-request-timeout=15m {{- end}} \
--enforce-node-allocatable= \
--eviction-hard= \
--cgroups-per-qos=false \
--fail-swap-on=false \
--authentication-token-webhook"
`

const KubeletConfigTmpl = `kind: KubeletConfiguration
apiVersion: kubelet.config.k8s.io/v1beta1
authentication:
  webhook:
    enabled: true
cgroupDriver: {{ if .UseLegacyCgroupDriver }}cgroupfs{{else}}systemd{{end}}
cgroupsPerQOS: false
enforceNodeAllocatable:
evictionHard:
failSwapOn: false
{{ if ne .ContainerRuntime "docker" -}}
runtimeRequestTimeout: 15m
NodeRegistration:
  CRISocket: {{.RuntimeEndpoint}}
{{- end}}
`
const KubeadmConfigTmpl = `apiVersion: kubeadm.k8s.io/v1alpha1
authorizationMode: AlwaysAllow
apiServerExtraArgs:
  insecure-port: "8080"
controllerManagerExtraArgs:
kubernetesVersion: {{.KubernetesVersion}}
schedulerExtraArgs:
{{if .ClusterCIDR -}}
kubeProxy:
  config:
    clusterCIDR: {{.ClusterCIDR}}
{{- end }}
{{if .PodNetworkCIDR -}}
networking:
  podSubnet: {{.PodNetworkCIDR}}
{{- end }}
{{if .HyperkubeImage -}}
unifiedControlPlaneImage: {{.HyperkubeImage}}
{{- end }}
`

const KubeSpawnRuncWrapperScript = `#!/bin/bash
# TODO: the docker-runc wrapper ensures --no-new-keyring is
# set, otherwise Docker will attempt to use keyring syscalls
# which are not allowed in systemd-nspawn containers. It can
# be removed once we require systemd v235 or later. We then
# will be able to whitelist the required syscalls; see:
# https://github.com/systemd/systemd/pull/6798
set -euo pipefail
args=()
for arg in "${@}"; do
	args+=("${arg}")
	if [[ "${arg}" == "create" ]] || [[ "${arg}" == "run" ]]; then
		args+=("--no-new-keyring")
	fi
done
exec docker-runc "${args[@]}"
`

// This matches "https://docs.projectcalico.org/v3.1/getting-started/kubernetes/installation
//               /hosted/kubernetes-datastore/calico-networking/1.7/calico.yaml"
// except that the ipam is overriden to use 2 ranges, 1 as expected
// in Calico felix, and one for ipv6. This has the affect of enabling dual-stack
// so that code in a Pod can bind to ::1 for lo.
const CalicoNet = `
# Calico Version v3.1.3
# https://docs.projectcalico.org/v3.1/releases#v3.1.3
# This manifest includes the following component versions:
#   calico/node:v3.1.3
#   calico/cni:v3.1.3

# This ConfigMap is used to configure a self-hosted Calico installation.
kind: ConfigMap
apiVersion: v1
metadata:
  name: calico-config
  namespace: kube-system
data:
  # To enable Typha, set this to "calico-typha" *and* set a non-zero value for Typha replicas
  # below.  We recommend using Typha if you have more than 50 nodes. Above 100 nodes it is
  # essential.
  typha_service_name: "none"

  # The CNI network configuration to install on each node.
  cni_network_config: |-
    {
      "name": "k8s-pod-network",
      "cniVersion": "0.3.0",
      "plugins": [
        {
          "type": "calico",
          "log_level": "info",
          "datastore_type": "kubernetes",
          "nodename": "__KUBERNETES_NODE_NAME__",
          "mtu": 1500,
          "ipam": {
            "type": "host-local",
            "ranges": [
                [
                {
                    "subnet": "192.168.0.0/16",
                    "rangeStart": "192.168.0.10",
                    "rangeEnd": "192.168.255.254"
                }
                ],
                [
                {
                    "subnet": "fc00::/64",
                    "rangeStart": "fc00:0:0:0:0:0:0:10",
                    "rangeEnd": "fc00:0:0:0:ffff:ffff:ffff:fffe"
                }
                ]
            ]
          },
          "policy": {
            "type": "k8s"
          },
          "kubernetes": {
            "kubeconfig": "__KUBECONFIG_FILEPATH__"
          }
        },
        {
          "type": "portmap",
          "snat": true,
          "capabilities": {"portMappings": true}
        }
      ]
    }

---

# This manifest creates a Service, which will be backed by Calico's Typha daemon.
# Typha sits in between Felix and the API server, reducing Calico's load on the API server.

apiVersion: v1
kind: Service
metadata:
  name: calico-typha
  namespace: kube-system
  labels:
    k8s-app: calico-typha
spec:
  ports:
    - port: 5473
      protocol: TCP
      targetPort: calico-typha
      name: calico-typha
  selector:
    k8s-app: calico-typha

---

# This manifest creates a Deployment of Typha to back the above service.

apiVersion: apps/v1beta1
kind: Deployment
metadata:
  name: calico-typha
  namespace: kube-system
  labels:
    k8s-app: calico-typha
spec:
  # Number of Typha replicas.  To enable Typha, set this to a non-zero value *and* set the
  # typha_service_name variable in the calico-config ConfigMap above.
  #
  # We recommend using Typha if you have more than 50 nodes.  Above 100 nodes it is essential
  # (when using the Kubernetes datastore).  Use one replica for every 100-200 nodes.  In
  # production, we recommend running at least 3 replicas to reduce the impact of rolling upgrade.
  replicas: 0
  revisionHistoryLimit: 2
  template:
    metadata:
      labels:
        k8s-app: calico-typha
      annotations:
        # This, along with the CriticalAddonsOnly toleration below, marks the pod as a critical
        # add-on, ensuring it gets priority scheduling and that its resources are reserved
        # if it ever gets evicted.
        scheduler.alpha.kubernetes.io/critical-pod: ''
    spec:
      hostNetwork: true
      tolerations:
        # Mark the pod as a critical add-on for rescheduling.
        - key: CriticalAddonsOnly
          operator: Exists
      # Since Calico can't network a pod until Typha is up, we need to run Typha itself
      # as a host-networked pod.
      serviceAccountName: calico-node
      containers:
      - image: quay.io/calico/typha:v0.7.4
        name: calico-typha
        ports:
        - containerPort: 5473
          name: calico-typha
          protocol: TCP
        env:
          # Enable "info" logging by default.  Can be set to "debug" to increase verbosity.
          - name: TYPHA_LOGSEVERITYSCREEN
            value: "info"
          # Disable logging to file and syslog since those don't make sense in Kubernetes.
          - name: TYPHA_LOGFILEPATH
            value: "none"
          - name: TYPHA_LOGSEVERITYSYS
            value: "none"
          # Monitor the Kubernetes API to find the number of running instances and rebalance
          # connections.
          - name: TYPHA_CONNECTIONREBALANCINGMODE
            value: "kubernetes"
          - name: TYPHA_DATASTORETYPE
            value: "kubernetes"
          - name: TYPHA_HEALTHENABLED
            value: "true"
          # Uncomment these lines to enable prometheus metrics.  Since Typha is host-networked,
          # this opens a port on the host, which may need to be secured.
          #- name: TYPHA_PROMETHEUSMETRICSENABLED
          #  value: "true"
          #- name: TYPHA_PROMETHEUSMETRICSPORT
          #  value: "9093"
        livenessProbe:
          httpGet:
            path: /liveness
            port: 9098
          periodSeconds: 30
          initialDelaySeconds: 30
        readinessProbe:
          httpGet:
            path: /readiness
            port: 9098
          periodSeconds: 10

---

# This manifest installs the calico/node container, as well
# as the Calico CNI plugins and network config on
# each master and worker node in a Kubernetes cluster.
kind: DaemonSet
apiVersion: extensions/v1beta1
metadata:
  name: calico-node
  namespace: kube-system
  labels:
    k8s-app: calico-node
spec:
  selector:
    matchLabels:
      k8s-app: calico-node
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 1
  template:
    metadata:
      labels:
        k8s-app: calico-node
      annotations:
        # This, along with the CriticalAddonsOnly toleration below,
        # marks the pod as a critical add-on, ensuring it gets
        # priority scheduling and that its resources are reserved
        # if it ever gets evicted.
        scheduler.alpha.kubernetes.io/critical-pod: ''
    spec:
      hostNetwork: true
      tolerations:
        # Make sure calico/node gets scheduled on all nodes.
        - effect: NoSchedule
          operator: Exists
        # Mark the pod as a critical add-on for rescheduling.
        - key: CriticalAddonsOnly
          operator: Exists
        - effect: NoExecute
          operator: Exists
      serviceAccountName: calico-node
      # Minimize downtime during a rolling upgrade or deletion; tell Kubernetes to do a "force
      # deletion": https://kubernetes.io/docs/concepts/workloads/pods/pod/#termination-of-pods.
      terminationGracePeriodSeconds: 0
      containers:
        # Runs calico/node container on each Kubernetes node.  This
        # container programs network policy and routes on each
        # host.
        - name: calico-node
          image: quay.io/calico/node:v3.1.3
          env:
            # Use Kubernetes API as the backing datastore.
            - name: DATASTORE_TYPE
              value: "kubernetes"
            # Enable felix info logging.
            - name: FELIX_LOGSEVERITYSCREEN
              value: "info"
            # Cluster type to identify the deployment type
            - name: CLUSTER_TYPE
              value: "k8s,bgp"
            # Disable file logging so 'kubectl logs' works.
            - name: CALICO_DISABLE_FILE_LOGGING
              value: "true"
            # Set Felix endpoint to host default action to ACCEPT.
            - name: FELIX_DEFAULTENDPOINTTOHOSTACTION
              value: "ACCEPT"
            # Disable IPV6 on Kubernetes.
            - name: FELIX_IPV6SUPPORT
              value: "false"
            # Set MTU for tunnel device used if ipip is enabled
            - name: FELIX_IPINIPMTU
              value: "1440"
            # Wait for the datastore.
            - name: WAIT_FOR_DATASTORE
              value: "true"
            # The default IPv4 pool to create on startup if none exists. Pod IPs will be
            # chosen from this range. Changing this value after installation will have
            # no effect. This should fall within '--cluster-cidr'.
            - name: CALICO_IPV4POOL_CIDR
              value: "192.168.0.0/16"
            # Enable IPIP
            - name: CALICO_IPV4POOL_IPIP
              value: "Always"
            # Enable IP-in-IP within Felix.
            - name: FELIX_IPINIPENABLED
              value: "true"
            # Typha support: controlled by the ConfigMap.
            - name: FELIX_TYPHAK8SSERVICENAME
              valueFrom:
                configMapKeyRef:
                  name: calico-config
                  key: typha_service_name
            # Set based on the k8s node name.
            - name: NODENAME
              valueFrom:
                fieldRef:
                  fieldPath: spec.nodeName
            # Auto-detect the BGP IP address.
            - name: IP
              value: "autodetect"
            - name: FELIX_HEALTHENABLED
              value: "true"
          securityContext:
            privileged: true
          resources:
            requests:
              cpu: 250m
          livenessProbe:
            httpGet:
              path: /liveness
              port: 9099
            periodSeconds: 10
            initialDelaySeconds: 10
            failureThreshold: 6
          readinessProbe:
            httpGet:
              path: /readiness
              port: 9099
            periodSeconds: 10
          volumeMounts:
            - mountPath: /lib/modules
              name: lib-modules
              readOnly: true
            - mountPath: /var/run/calico
              name: var-run-calico
              readOnly: false
            - mountPath: /var/lib/calico
              name: var-lib-calico
              readOnly: false
        # This container installs the Calico CNI binaries
        # and CNI network config file on each node.
        - name: install-cni
          image: quay.io/calico/cni:v3.1.3
          command: ["/install-cni.sh"]
          env:
            # Name of the CNI config file to create.
            - name: CNI_CONF_NAME
              value: "10-calico.conflist"
            # The CNI network config to install on each node.
            - name: CNI_NETWORK_CONFIG
              valueFrom:
                configMapKeyRef:
                  name: calico-config
                  key: cni_network_config
            # Set the hostname based on the k8s node name.
            - name: KUBERNETES_NODE_NAME
              valueFrom:
                fieldRef:
                  fieldPath: spec.nodeName
          volumeMounts:
            - mountPath: /host/opt/cni/bin
              name: cni-bin-dir
            - mountPath: /host/etc/cni/net.d
              name: cni-net-dir
      volumes:
        # Used by calico/node.
        - name: lib-modules
          hostPath:
            path: /lib/modules
        - name: var-run-calico
          hostPath:
            path: /var/run/calico
        - name: var-lib-calico
          hostPath:
            path: /var/lib/calico
        # Used to install CNI.
        - name: cni-bin-dir
          hostPath:
            path: /opt/cni/bin
        - name: cni-net-dir
          hostPath:
            path: /etc/cni/net.d

# Create all the CustomResourceDefinitions needed for
# Calico policy and networking mode.
---

apiVersion: apiextensions.k8s.io/v1beta1
kind: CustomResourceDefinition
metadata:
   name: felixconfigurations.crd.projectcalico.org
spec:
  scope: Cluster
  group: crd.projectcalico.org
  version: v1
  names:
    kind: FelixConfiguration
    plural: felixconfigurations
    singular: felixconfiguration

---

apiVersion: apiextensions.k8s.io/v1beta1
kind: CustomResourceDefinition
metadata:
  name: bgppeers.crd.projectcalico.org
spec:
  scope: Cluster
  group: crd.projectcalico.org
  version: v1
  names:
    kind: BGPPeer
    plural: bgppeers
    singular: bgppeer

---

apiVersion: apiextensions.k8s.io/v1beta1
kind: CustomResourceDefinition
metadata:
  name: bgpconfigurations.crd.projectcalico.org
spec:
  scope: Cluster
  group: crd.projectcalico.org
  version: v1
  names:
    kind: BGPConfiguration
    plural: bgpconfigurations
    singular: bgpconfiguration

---

apiVersion: apiextensions.k8s.io/v1beta1
kind: CustomResourceDefinition
metadata:
  name: ippools.crd.projectcalico.org
spec:
  scope: Cluster
  group: crd.projectcalico.org
  version: v1
  names:
    kind: IPPool
    plural: ippools
    singular: ippool

---

apiVersion: apiextensions.k8s.io/v1beta1
kind: CustomResourceDefinition
metadata:
  name: hostendpoints.crd.projectcalico.org
spec:
  scope: Cluster
  group: crd.projectcalico.org
  version: v1
  names:
    kind: HostEndpoint
    plural: hostendpoints
    singular: hostendpoint

---

apiVersion: apiextensions.k8s.io/v1beta1
kind: CustomResourceDefinition
metadata:
  name: clusterinformations.crd.projectcalico.org
spec:
  scope: Cluster
  group: crd.projectcalico.org
  version: v1
  names:
    kind: ClusterInformation
    plural: clusterinformations
    singular: clusterinformation

---

apiVersion: apiextensions.k8s.io/v1beta1
kind: CustomResourceDefinition
metadata:
  name: globalnetworkpolicies.crd.projectcalico.org
spec:
  scope: Cluster
  group: crd.projectcalico.org
  version: v1
  names:
    kind: GlobalNetworkPolicy
    plural: globalnetworkpolicies
    singular: globalnetworkpolicy

---

apiVersion: apiextensions.k8s.io/v1beta1
kind: CustomResourceDefinition
metadata:
  name: globalnetworksets.crd.projectcalico.org
spec:
  scope: Cluster
  group: crd.projectcalico.org
  version: v1
  names:
    kind: GlobalNetworkSet
    plural: globalnetworksets
    singular: globalnetworkset

---

apiVersion: apiextensions.k8s.io/v1beta1
kind: CustomResourceDefinition
metadata:
  name: networkpolicies.crd.projectcalico.org
spec:
  scope: Namespaced
  group: crd.projectcalico.org
  version: v1
  names:
    kind: NetworkPolicy
    plural: networkpolicies
    singular: networkpolicy

---

apiVersion: v1
kind: ServiceAccount
metadata:
  name: calico-node
  namespace: kube-system
`
