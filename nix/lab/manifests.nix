# Applied by k3s on lab-1 at start: Longhorn (Helm chart) and test apps.
let
  ns = name: { apiVersion = "v1"; kind = "Namespace"; metadata.name = name; };
  pvc = namespace: name: size: {
    apiVersion = "v1"; kind = "PersistentVolumeClaim";
    metadata = { inherit name namespace; };
    spec = { accessModes = [ "ReadWriteOnce" ]; storageClassName = "longhorn"; resources.requests.storage = size; };
  };
  # a container that writes the time to its volume every 10 s
  writer = mount: {
    name = "app";
    image = "docker.io/library/busybox:1.37";
    command = [ "sh" "-c" "while true; do date >> ${mount}/log; sleep 10; done" ];
    volumeMounts = [{ name = "data"; mountPath = mount; }];
  };
in
{
  longhorn.content = {
    apiVersion = "helm.cattle.io/v1";
    kind = "HelmChart";
    metadata = { name = "longhorn"; namespace = "kube-system"; };
    spec = {
      repo = "https://charts.longhorn.io";
      chart = "longhorn";
      version = "1.12.1";
      targetNamespace = "longhorn-system";
      createNamespace = true;
      valuesContent = builtins.toJSON {
        defaultBackupStore.backupTarget = "nfs://10.77.0.11:/export/backups";
        persistence.defaultClassReplicaCount = 2;
        defaultSettings.defaultReplicaCount = 2;
        preUpgradeChecker.jobEnabled = false;
        longhornUI.replicas = 0;
        csi = { attacherReplicaCount = 1; provisionerReplicaCount = 1; resizerReplicaCount = 1; snapshotterReplicaCount = 1; };
      };
    };
  };

  # test apps: a Deployment with a PVC and hooks on the Deployment, a
  # StatefulSet with two replicas (ordered stop), a bare PVC without a pod
  apps.content = [
    (ns "lab-apps")
    (pvc "lab-apps" "web-data" "1Gi")
    {
      apiVersion = "apps/v1"; kind = "Deployment";
      metadata = {
        name = "web"; namespace = "lab-apps";
        annotations = {
          "lhtx.io/pre-freeze" = "app: echo pre-freeze hook ran >> /data/log; sync";
          "lhtx.io/post-unfreeze" = "app: echo post-unfreeze hook ran >> /data/log";
        };
      };
      spec = {
        replicas = 1;
        selector.matchLabels.app = "web";
        template = {
          metadata.labels.app = "web";
          spec = {
            containers = [ (writer "/data") ];
            volumes = [{ name = "data"; persistentVolumeClaim.claimName = "web-data"; }];
          };
        };
      };
    }
    {
      apiVersion = "apps/v1"; kind = "StatefulSet";
      metadata = { name = "db"; namespace = "lab-apps"; };
      spec = {
        replicas = 2;
        serviceName = "db";
        selector.matchLabels.app = "db";
        template = {
          metadata.labels.app = "db";
          spec.containers = [ (writer "/var/lib/db") ];
        };
        volumeClaimTemplates = [{
          metadata.name = "data";
          spec = { accessModes = [ "ReadWriteOnce" ]; storageClassName = "longhorn"; resources.requests.storage = "1Gi"; };
        }];
      };
    }
    (pvc "lab-apps" "spare" "1Gi")
  ];

  # encrypted volumes (dm-crypt with a key from a Kubernetes Secret), both
  # ways Longhorn documents: one global Secret, or one Secret per PVC
  crypto.content =
    let
      key = value: {
        CRYPTO_KEY_VALUE = value; # lab only – never a real key
        CRYPTO_KEY_PROVIDER = "secret";
        CRYPTO_KEY_CIPHER = "aes-xts-plain64";
        CRYPTO_KEY_HASH = "sha256";
        CRYPTO_KEY_SIZE = "256";
        CRYPTO_PBKDF = "argon2i";
      };
      secret = namespace: name: value: {
        apiVersion = "v1"; kind = "Secret"; type = "Opaque";
        metadata = { inherit name namespace; };
        stringData = key value;
      };
      sc = name: secretName: secretNamespace: {
        apiVersion = "storage.k8s.io/v1"; kind = "StorageClass";
        metadata.name = name;
        provisioner = "driver.longhorn.io";
        allowVolumeExpansion = true;
        reclaimPolicy = "Delete";
        volumeBindingMode = "Immediate";
        parameters = {
          numberOfReplicas = "2";
          encrypted = "true";
        } // builtins.listToAttrs (map (k: { name = "csi.storage.k8s.io/${k}-secret-name"; value = secretName; })
          [ "provisioner" "node-publish" "node-stage" "node-expand" ])
          // builtins.listToAttrs (map (k: { name = "csi.storage.k8s.io/${k}-secret-namespace"; value = secretNamespace; })
          [ "provisioner" "node-publish" "node-stage" "node-expand" ]);
      };
      app = name: claim: {
        apiVersion = "apps/v1"; kind = "Deployment";
        metadata = { inherit name; namespace = "lab-crypto"; };
        spec = {
          replicas = 1;
          selector.matchLabels.app = name;
          template = {
            metadata.labels.app = name;
            spec = {
              containers = [ (writer "/data") ];
              volumes = [{ name = "data"; persistentVolumeClaim.claimName = claim; }];
            };
          };
        };
      };
      cpvc = name: sc: (pvc "lab-crypto" name "1Gi") // {
        spec = { accessModes = [ "ReadWriteOnce" ]; storageClassName = sc; resources.requests.storage = "1Gi"; };
      };
    in
    [
      (ns "lab-crypto")
      (secret "longhorn-system" "longhorn-crypto" "lab-global-key-not-secret")
      (sc "longhorn-crypto-global" "longhorn-crypto" "longhorn-system")
      (sc "longhorn-crypto-per-pvc" "\${pvc.name}" "\${pvc.namespace}")
      (secret "lab-crypto" "vault-data" "lab-per-pvc-key-not-secret")
      (cpvc "notes-data" "longhorn-crypto-global")
      (app "notes" "notes-data")
      (cpvc "vault-data" "longhorn-crypto-per-pvc")
      (app "vault" "vault-data")
    ];
}
