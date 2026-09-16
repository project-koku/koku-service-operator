# Cost Management Service Operator

## What is Red Hat Lightspeed cost management?
Red Hat Lightspeed cost management simplifies managing resources and costs across cloud and OpenShift Container Platform environments, helping system administrators optimize spend and align IT costs with business priorities.

You can use cost management to perform the following tasks to help your organization optimize costs, increase efficiency, and save money:

Visualize, understand, and analyze how your resources are used.
Track cost trends.
Break down charges to your projects and organizations.
Use cost models to apply cost to OpenShift usage metrics or add markups, or both.
Forecast your future consumption.
Identify patterns of usage that you might want to investigate.
Export data to integrate with third-party tools.


## How can I use Red Hat Lightspeed cost management?
Red Hat has been offering a managed version of cost management for years. For more information on the managed version of cost management, see the Red Hat Lightspeed cost management documentation.

Red Hat is now introducing a self-managed version of cost management. This version is a fully local, self-managed deployment of the Red Hat Lightspeed cost management service running directly inside your own cluster. With this deployment of the cost management service, you are fully in control of your own data and your configuration.


## What are the advantages of the self-managed version of the cost management service?

Enables highly regulated, disconnected networks (defense, government, finance) to track infrastructure spending without violating strict internet bans.

Ensures sensitive data, cluster metrics, and internal workload labels never cross the corporate firewall.

Enables cost management for organizations that require self-managed infrastructure but need the same visibility available in SaaS deployments.

NOTE: The process of setting up self-managed cost management requires you to bring in several aspects of your own external infrastructure. These infrastructure elements include `PostgreSQL 16`, `cache`, `Kafka`, `S3-compatible object storage`, and `OIDC`.
