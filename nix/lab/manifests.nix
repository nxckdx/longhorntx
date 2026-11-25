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
}
