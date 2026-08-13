# Declarative Configuration and Future DSL

## 1. Start with a stable IR, not a custom syntax

The declarative system should first define a versioned intermediate representation (IR) in Go structs + JSON/YAML schema.

Only after the resource semantics stabilize should a custom DSL be introduced.

This avoids freezing syntax before the control-plane concepts are proven.

## 2. Example IR

```yaml
apiVersion: fleetplane.io/v1alpha1
kind: Pool
metadata:
  name: ci
spec:
  resourceKind: compute.machine
  providerSelector:
    match:
      region: eu
  template:
    class: ci-large
  replicas: 4
  policy:
    minReady: 1
    maxResources: 20
    idleTTL: 10m
```

## 3. Future DSL goals

A DSL may provide:
- concise pool definitions;
- reusable templates;
- expressions;
- provider selection;
- policies;
- module-defined blocks;
- static validation;
- compilation to the canonical IR.

## 4. Extensible syntax

Resource modules should be able to contribute schema rather than forcing the core parser to understand every future resource.

Conceptually:

```text
pool "ci" {
  resource compute.machine {
    provider = hetzner.main
    image    = "runner-v12"
    cpu      >= 2
    memory   >= 8Gi
  }

  replicas = 4
}
```

A database module could introduce different fields without modifying the core:

```text
resource database.postgres {
  storage >= 100Gi
  version = "18"
}
```

## 5. Rust-style macros are not required

Compile-time Go modules can register resource schemas, validators and planners. The user-facing DSL can be implemented independently of the host language and compile to the same IR.

This preserves the option to make the DSL sophisticated later without making Rust procedural macros a prerequisite for the control plane.

## 6. Versioning

Every manifest/IR object must carry an API version. Conversion functions should permit evolution:

```text
v1alpha1 -> v1beta1 -> v1
```

Persist canonical versions and retain unknown provider/module extension fields when round-tripping.
