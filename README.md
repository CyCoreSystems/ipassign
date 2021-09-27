# ipassign

[![PkgGoDev](https://pkg.go.dev/badge/github.com/CyCoreSystems/ipassign)](https://pkg.go.dev/github.com/CyCoreSystems/ipassign)

This package is published as a container to `ghcr.io/cycoresystems/ipassign`.

`ipassign` binds a given set of kubernetes Nodes to a given set of Public IP
addresses.  Both sets are determined by tags, where the Nodes use kubernetes
labels, and the Public IPs use the cloud provider's selection mechanism (labels
or tags).

In AWS, Elastic IPs are additionally selected by a `GROUP` tag.  This is to facilitate
deployment groups while maintaining the same resource tags otherwise.  For GCP,
this kind of layering is usually applied through the use of differing deployment
zones or differing project IDs.

This utility is somewhat opinionated, but the backends are now modular, allowing
you to create more flexible options, should you need them.

## RBAC

Because the Node object is not a namespaced resource, a simple RoleBinding is
not sufficient.  You must use ClusterRole and ClusterRoleBindings.

Please see the [example](ipassign.yaml).
