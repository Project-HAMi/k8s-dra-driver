# HAMi DRA Driver

## Overview

DRA is a Kubernetes feature that lets you request and share resources among Pods, it provides a flexible way for requesting, configuring, and sharing specialized devices like GPUs. To learn more about DRA in general, good starting points are: [Kubernetes docs](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/), [Kubernetes blog](https://kubernetes.io/blog/2025/05/01/kubernetes-v1-33-dra-updates/).

The HAMi DRA Driver brings the support of DRA for [HAMi-Core](https://github.com/Project-HAMi/HAMi-core)(the in-container gpu resource controller).


## Installation

Please check the [DaemonSet configuration](demo/yaml/ds.yaml) in the demo/yaml directory. We are working on a Helm Chart for this DRA Driver, it will be available in the near future.


## Demo

Please refer to the [demo.md](demo/demo.md) for more information.

## Contributing

Contributions require a Developer Certificate of Origin (DCO, see [CONTRIBUTING.md](https://github.com/Project-HAMi/HAMi/blob/master/CONTRIBUTING.md).

## Support

Please open an issue on the GitHub project for questions and for reporting problems.
Your feedback is appreciated!
