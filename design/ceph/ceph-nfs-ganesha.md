# Ceph NFS-Ganesha CRD

[NFS-Ganesha] is a user space NFS server that is well integrated with [CephFS]
and [RGW] backends. It can export Ceph's filesystem namespaces and Object
gateway namespaces over NFSv4 protocol.

Rook already orchestrates Ceph filesystem and Object store (or RGW) on
Kubernetes (k8s). It can be extended to orchestrate NFS-Ganesha server daemons
as highly available and  scalable NFS gateway pods to the Ceph filesystem and
Object Store. This will allow NFS client applications to use the Ceph filesystem
and object store setup by rook.

This feature mainly differs from the feature to add NFS as an another
storage backend for rook (the general NFS solution) in the following ways:

* It will use the rook's Ceph operator and not a separate NFS operator to
  deploy the NFS server pods.

* The NFS server pods will be directly configured with CephFS or RGW
  backend setup by rook, and will not require CephFS or RGW to be mounted
  in the NFS server pod with a PVC.

## Design of Ceph NFS-Ganesha CRD

The NFS-Ganesha server settings will be exposed to Rook as a
Custom Resource Definition (CRD). Creating the nfs-ganesha CRD will launch
a cluster of NFS-Ganesha server pods that will be configured with no exports.

The NFS client recovery data will be stored in a Ceph RADOS pool; and
the servers will have stable IP addresses by using [k8s Service].
Export management will be done by updating a per-pod config file object
in RADOS by external tools and issuing dbus commands to the server to
reread the configuration.

This allows the NFS-Ganesha server cluster to be scalable and highly available.

### Prerequisites

- A running rook Ceph filesystem or object store, whose namespaces will be
  exported by the NFS-Ganesha server cluster.
  e.g.,
  ```
  kubectl create -f deploy/examples/filesystem.yaml
  ```

- An existing RADOS pool (e.g., CephFS's data pool) or a pool created with a
  [Ceph Pool CRD] to store NFS client recovery data.

### Ceph NFS-Ganesha CRD

#### Server settings
- Number of active Ganesha servers in the cluster
- Placement of the Ganesha servers
- Resource limits (memory, CPU) of the Ganesha server pods

#### Security settings

- Using [SSSD] (System Security Services Daemon)
  - This can be used to connect to many ID mapping services, but only LDAP has been tested
  - Run SSSD in a sidecar
    - The sidecar uses a different container image than ceph, and users should be able to specify it
    - Resources requests/limits should be configurable for the sidecar container
    - Users must be able to specify the SSSD config file
      - Users can add an SSSD config from any standard Kubernetes VolumeMount
        - Older SSSD versions (like those available in CentOS 7) do not support loading config files
          from `/etc/sssd/conf.d/*`; they must use `/etc/sssd/sssd.conf`. Newer versions support
          either method.
        - To make configuration as simple as possible to document for users, we only support the
          `/etc/sssd/sssd.conf` method. This may reduce some configurability, but it is much simpler
          to document for users. For an option that is already complex, the simplicity here is a value.
        - We allow users to specify any VolumeSource. There are two caveats:
          1. The file be mountable as `sssd.conf` via a VolumeMount `subPath`, which is how Rook
             will mount the file into the SSSD sidecar.
          2. The file mode must be 0600.
        - Users only need one SSSD conf Volume per CephNFS that has this option path enabled.

#### Example
Below is an example NFS-Ganesha CRD, `nfs-ganesha.yaml`

```yaml
apiVersion: ceph.rook.io/v1
kind: CephNFS
metadata:
  # The name of Ganesha server cluster to create. It will be reflected in
  # the name(s) of the ganesha server pod(s)
  name: mynfs
  # The namespace of the Rook cluster where the Ganesha server cluster is
  # created.
  namespace: rook-ceph
spec:
  # Settings for the ganesha server
  server:
    # the number of active ganesha servers
    active: 3
    # where to run the nfs ganesha server
    placement:
    #  nodeAffinity:
    #    requiredDuringSchedulingIgnoredDuringExecution:
    #      nodeSelectorTerms:
    #      - matchExpressions:
    #        - key: role
    #          operator: In
    #          values:
    #          - mds-node
    #  tolerations:
    #  - key: mds-node
    #    operator: Exists
    #  podAffinity:
    #  podAntiAffinity:
    # The requests and limits set here allow the ganesha pod(s) to use half of
    # one CPU core and 1 gigabyte of memory
    resources:
    #  limits:
    #    cpu: "3"
    #    memory: "8Gi"
    #  requests:
    #    cpu: "3"
    #    memory: "8Gi"
    # the priority class to set to influence the scheduler's pod preemption
    priorityClassName:

  security:
    sssd:
      sidecar:
        image: registry.access.redhat.com/rhel7/sssd:latest
        sssdConfigFile:
          volumeSource: # any standard kubernetes volume source
            # example
            configMap:
              name: rook-ceph-nfs-organization-sssd-config
              defaultMode: 0600 # mode must be 0600
        resources:
          # requests:
          #   cpu: "2"
          #   memory: "1024Mi"
          # limits:
          #   cpu: "2"
          #   memory: "1024Mi"

    kerberos:
      principalName: nfs
      configFiles:
        volumeSource:
          configMap:
            name: rook-ceph-nfs-organization-krb-conf
      keytabFile:
        volumeSource:
          secret:
            name: rook-ceph-nfs-organization-keytab


```

When the  nfs-ganesha.yaml is created the following will happen:

- Rook's Ceph operator sees the creation of the NFS-Ganesha CRD.

- The operator creates as many [k8s Deployments] as the number of active
  Ganesha servers mentioned in the CRD. Each deployment brings up a Ganesha
  server pod, a replicaset of size 1.

- The ganesha servers, each running in a separate pod, use a mostly-identical
  ganesha config (ganesha.conf) with no EXPORT definitions. The end of the
  file will have it do a %url include on a pod-specific RADOS object from which
  it reads the rest of its config.

- The operator creates a k8s service for each of the ganesha server pods
  to allow each of the them to have a stable IP address.

The ganesha server pods constitute an active-active high availability NFS
server cluster. If one of the active Ganesha server pods goes down, k8s brings
up a replacement ganesha server pod with the same configuration and IP address.
The NFS server cluster can be scaled up or down by updating the
number of the active Ganesha servers in the CRD (using `kubectl edit` or
modifying the original CRD and running `kubectl apply -f <CRD yaml file>`).

### Per-node config files
After loading the basic ganesha config from inside the container, the node
will read the rest of its config from an object in RADOS. This allows external
tools to generate EXPORT definitions for ganesha.

The object will be named "conf-<metadata.name>.<index>", where metadata.name
is taken from the CRD and the index is internally generated. It will be stored
in `rados.pool` and `rados.namespace` from the above CRD.

### Consuming the NFS shares

An external consumer will fetch the ganesha server IPs by querying the k8s
services of the Ganesha server pods. It should have network access to the
Ganesha pods to manually mount the shares using a NFS client. Later, support
will be added to allow user pods to easily consume the NFS shares via PVCs.

## Example use-case

The NFS shares exported by rook's ganesha server pods can be consumed by
[OpenStack] cloud's user VMs. To do this, OpenStack's shared file system
service, [Manila] will provision NFS shares backed by CephFS using rook.
Manila's [CephFS driver] will create NFS-Ganesha CRDs to launch ganesha server
pods. The driver will dynamically add or remove exports of the ganesha server
pods based on OpenStack users' requests. The OpenStack user VMs will have
network connectivity to the ganesha server pods, and manually mount the shares
using NFS clients.

## Detailed designs
### DBus
NFS-Ganesha requires DBus. Run DBus as a sidecar container so that it can be restarted if the
process fails. The `/run/dbus` directory must be shared between Ganesha and DBus.

### [SSSD] (System Security Services Daemon)
SSSD is able to provide user ID mapping to NFS-Ganesha. It can integrate with LDAP, Active
Directory, and FreeIPA.

Prototype information detailed on Rook blog:
https://blog.rook.io/prototyping-an-nfs-connection-to-ldap-using-sssd-7c27f624f1a4

NFS-Ganesha (via libraries within its container) is the client to SSSD. As of Ceph v17.2.3, the Ceph
container image does not have the `sssd-client` package installed which is required for supporting
SSSD. It should be available in Ceph v17.2.4.

The following directories must be shared between SSSD and the NFS-Ganesha container:
- `/var/lib/sss/pipes`: this directory holds the sockets used to communicate between client and SSSD
- `/var/lib/sss/mc`: this is a memory-mapped "L0" cache shared between client and SSSD

The following directories should **not** be shared between SSSD and other containers:
- `/var/lib/sss/db`: this is a memory-mapped "L1" cache that is intended to survive reboots
- `/run/dbus`: using the DBus instance from the sidecar caused SSSD errors in testing. SSSD only
  uses DBus for internal communications and creates its own socket as needed.

### Kerberos
**NOTE:** The principles behind this design have not been validated.

#### Volumes
Volumes that should be mounted into nfs-ganesha container to support Kerberos:
1. `keytabFile` volume: use `subPath` on the mount to add the `krb5.keytab` file to `/etc/krb5.keytab`
2. `configFiles` volume: mount (without `subPath`) to `/etc/krb5.conf.rook/` to allow all files to
   be mounted (e.g., if a ConfigMap has multiple data items or hostPath has multiple conf.d files)

#### Ganesha config
Should add configuration. Docs say Active_krb 5 is default true if krb support is compiled in, but
most examples have this explicitly set.
Default PrincipalName is "nfs".
Default for keytab path is reportedly empty. Rook can use `/etc/krb5.keytab`.

Create a new RADOS object named `kerberos` to configure Kerberos.
```ini
NFS_KRB5
{
   PrincipalName = nfs ; # <-- set from spec.security.kerberos.principalName
   KeytabPath = /etc/krb5.keytab ;
   Active_krb5 = YES ;
}
```

Add the following line to to the config object (`conf-nfs.${nfs-name}`) to reference the new
`kerberos` RADOS object. Remove this line from the config object if Kerberos is disabled.
```
%url "rados://.nfs/${nfs-name}/kerberos"
```

These steps can be done from the Rook operator.

#### Kerberos config
Below is the `/etc/krb5.conf` default from the ceph container:
```ini
# To opt out of the system crypto-policies configuration of krb5, remove the
# symlink at /etc/krb5.conf.d/crypto-policies which will not be recreated.
includedir /etc/krb5.conf.d/

[logging]
    default = FILE:/var/log/krb5libs.log
    kdc = FILE:/var/log/krb5kdc.log
    admin_server = FILE:/var/log/kadmind.log

[libdefaults]
    dns_lookup_realm = false
    ticket_lifetime = 24h
    renew_lifetime = 7d
    forwardable = true
    rdns = false
    pkinit_anchors = FILE:/etc/pki/tls/certs/ca-bundle.crt
    spake_preauth_groups = edwards25519
#    default_realm = EXAMPLE.COM
    default_ccache_name = KEYRING:persistent:%{uid}

[realms]
# EXAMPLE.COM = {
#     kdc = kerberos.example.com
#     admin_server = kerberos.example.com
# }

[domain_realm]
# .example.com = EXAMPLE.COM
# example.com = EXAMPLE.COM
```

Rook should set its own krb.conf like so. Rook can create `krb5.conf` in an EmptyDir from an init
container and mount it to the `nfs-ganesha` container in the same fashion as for `nsswitch.conf`.
Inline comments explain changes made compared to the default file.
```ini
includedir /etc/krb5.conf.d/    # assume we want to keep crypto-policies config
includedir /etc/krb5.conf.rook/ # load configFiles content from users

[logging]
    default = STDERR # only log to stderr by default

# remove all other defaults (namely 'libdefaults') so that users have full flexibility to set what they want.
```

**QUESTION:** Do we want to keep this `crypto-policies` file? Do we think users might want to remove
it? Will overriding `[libdefaults] permitted_enctypes` be sufficient? Will they have to name their
file `zz-*` to ensure it overwrites `crypto-policies`?
```
$# cat /etc/krb5.conf.d/crypto-policies
[libdefaults]
permitted_enctypes = aes256-cts-hmac-sha1-96 aes256-cts-hmac-sha384-192 camellia256-cts-cmac aes128-cts-hmac-sha1-96 aes128-cts-hmac-sha256-128 camellia128-cts-cmac
```

#### Enabling kerberos for an export
**QUESTION:**
`sectype` enables Kerberos for an export and is set on the Export definition. I don't see that
option in `ceph nfs export ...` commands. Do we need to manually add `sectype` to all exports?
How will CSI do this? Should be part of Ceph API, no?

Example export (with `sectype` manually added):
```ini
EXPORT {
    FSAL {
        name = "CEPH";
        user_id = "nfs.my-nfs.1";
        filesystem = "myfs";
        secret_access_key = "AQBsPf1iNXTRKBAAtw+D5VzFeAMV4iqbfI0IBA==";
    }
    export_id = 1;
    path = "/";
    pseudo = "/test";
    access_type = "RW";
    squash = "none";
    attr_expiration_time = 0;
    security_label = true;
    protocols = 4;
    transports = "TCP";
    sectype = krb5,krb5i,krb5p; # <-- not included in ceph nfs exports by default
}
```


<!--------------------------------------------- LINKS --------------------------------------------->
[NFS-Ganesha]: https://github.com/nfs-ganesha/nfs-ganesha/wiki
[CephFS]: http://docs.ceph.com/docs/master/cephfs/nfs/
[RGW]: http://docs.ceph.com/docs/master/radosgw/nfs/
[Rook toolbox]: (/Documentation/ceph-toolbox.md)
[Ceph manager]: (http://docs.ceph.com/docs/master/mgr/)
[OpenStack]: (https://www.openstack.org/software/)
[Manila]: (https://wiki.openstack.org/wiki/Manila)
[CephFS driver]: (https://github.com/openstack/manila/blob/master/doc/source/admin/cephfs_driver.rst)
[k8s ConfigMaps]: (https://kubernetes.io/docs/tasks/configure-pod-container/configure-pod-configmap/)
[k8s Service]: (https://kubernetes.io/docs/concepts/services-networking/service)
[Ceph Pool CRD]: (https://github.com/rook/rook/blob/master/Documentation/ceph-pool-crd.md)
[k8s Deployments]: (https://kubernetes.io/docs/concepts/workloads/controllers/deployment/)
[SSSD]: (https://sssd.io)
[PKINIT]: https://web.mit.edu/kerberos/krb5-devel/doc/admin/pkinit.html#pkinit
