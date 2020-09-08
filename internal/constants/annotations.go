package constants

// AnnotationAssignment is the Node annotation which record the Public IP which has been assigned to the Node.
// Because there may be a delay between assignment and activiation, this is used by the Reconciler to ignore Nodes which have already been processed.
const AnnotationAssignment = "ipassign.cycore.io/assigned-ip"
