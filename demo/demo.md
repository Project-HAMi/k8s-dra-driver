# A (kind) demo for HAMi DRA Driver

## Prerequisite

Running this demo requires the following software:
* NVIDIA GPU Driver & CUDA
* Docker & NVIDIA Container Toolkit & Runtime
* kind (follow the official [installation docs](https://kind.sigs.k8s.io/docs/user/quick-start/#installation))

## Build Image

```Bash
git clone https://github.com/Project-HAMi/k8s-dra-driver.git
cd k8s-dra-driver
make image
```

After building, you can find the projecthami/k8s-dra-driver:v0.0.1-dev image with `docker image ls` command.

## Configure container runtime

Configure the NVIDIA Container Runtime as the default runtime:
```Bash
sudo nvidia-ctk runtime configure --runtime={docker|containerd|other runtime} --set-as-default
```

Restart the container service to apply the changes
```Bash
# Take docekr as an example
sudo systemctl restart docker 
```

Set the accept-nvidia-visible-devices-as-volume-mounts option to true in the /etc/nvidia-container-runtime/config.toml file to configure the NVIDIA Container Runtime to use volume mounts to select devices to inject into a container.
```Bash
sudo nvidia-ctk config --in-place --set accept-nvidia-visible-devices-as-volume-mounts=true
```

## Create cluster with kind

The environments in the `demo/clusters/kind/scripts/common.sh` script can be modified to customize the cluster, for example setting the `KIND_K8S_TAG` environment to specify the Kubernetes version of cluster.

Increase the `role: worker` in [kind-cluster-config.yaml](demo/clusters/kind/scripts/kind-cluster-config.yaml) for a multi-node cluster and check https://kind.sigs.k8s.io/docs/user/configuration for more configurations about kind

Finally, Please make sure that the `featureGates.DRAConsumableCapacity` in the [kind-cluster-config.yaml](demo/clusters/kind/scripts/kind-cluster-config.yaml) has been set to true, before bring up the cluster.

```Bash
./demo/clusters/kind/create-cluster.sh
```

After create the cluster, `projecthami/k8s-dra-driver:v0.0.1-dev` image will be loaded into nodes automatically.

## Install HAMi DRA Driver

```Bash
cd demo/yaml
kubectl apply -f rbac.yaml
kubectl apply -f ds.yaml
```

## Experience HAMi-Core DRA

Images should be loaded into cluster before creating the pod first.

```Bash
docker pull ubuntu:24.04
kind load docker-image ubuntu:24.04 --name k8s-dra-driver-cluster
```

Then setup the DeviceClass and ResourceClaim and create pods.

```Bash
kubectl apply -f setup.yaml
kubectl create -f pod-0.yaml
```

Please check the files in demo/yaml for configure DRA with ResourceClaimTemplate

## Cleanup
```Bash
./demo/clusters/kind/delete-cluster.sh
```
