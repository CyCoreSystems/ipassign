# ipassign

`ipassign` binds a given set of kubernetes Nodes to a given set of AWS Elastic IP
addresses.  Both sets are determined by tags, where the Nodes use kubernetes
labels, and the Elastic IPs are defined by AWS resource Tags.

Elastic IPs are additionally selected by a `GROUP` tag.  This is to facilitate
deployment groups while maintaining the same resource tags otherwise.

This is meant more as an example than a direct-use utility, as the specifics of
your deployment may vary greatly.

