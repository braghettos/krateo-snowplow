# Decoupling [`authn`][authn] from `snowplow` for Testing and Operations

> Architecture Decision Record (ADR): 2025-10-22 


## Context

Service `snowplow` depends on the [`authn`][authn] service for authentication and token issuance.  
Both are deployed on Kubernetes and use custom setup and CRDs, which makes isolated testing complex and fragile.  
Setting up [`authn`][authn] just to test `snowplow` introduces unnecessary overhead, frequent configuration issues, and slows down iteration.


## Decision

Use the existing [`krateoctl`][krateoctl] tool (already used and distributed across environments) with the new command:  `krateoctl add-user`.

This command performs a _one-time user registration and token generation_ using the shared authentication library, without requiring the [`authn`][authn] service to be deployed.

It enables developers — and admins — to create valid users and obtain Bearer tokens directly from the CLI.


## Consequences

- ✅ **Simplified testing:** Services that depend on authentication can now be tested independently of [`authn`][authn].  
- ✅ **Reduced operational overhead:** No need to deploy or maintain [`authn`][authn] in local or CI environments.  
- ✅ **Consistency:** The command reuses the shared auth library, ensuring compatibility with real authentication flows.  
- ✅ **Admin utility:** The same tool helps administrators quickly bootstrap or manage users.  


## Outcome

This decision improves service isolation, test reliability, and development velocity, while maintaining alignment with the actual authentication mechanism.

It follows microservice testing best practices by reducing inter-service coupling and leveraging existing operational tools.


[authn]: https://github.com/krateoplatformops/authn
[krateoctl]: https://github.com/krateoplatformops/krateoctl/releases
