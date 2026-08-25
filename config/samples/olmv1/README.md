# OLMv1 ClusterExtension sample (OCP 5)

OLMv0 (CatalogSource + Subscription on OCP 4.x) remains supported. Use this
sample only on clusters that have `olm.operatorframework.io/v1`.

`spec.namespace` is the **operator pod** namespace. Omit
`spec.config.inline.watchNamespace` so the extension watches all namespaces
(AllNamespaces). BYOI infra may live elsewhere.

See [docs/development/allnamespaces.md](../../../docs/development/allnamespaces.md).
