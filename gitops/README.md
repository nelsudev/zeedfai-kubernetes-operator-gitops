# GitOps

Flux structure:

```
gitops/
├── clusters/
│   ├── staging/        # flux bootstrap points here
│   └── prod/
└── infrastructure/     # HelmReleases: strimzi, kube-prometheus-stack, operator
```

Bootstrap (once the repo is on GitHub):

```bash
flux bootstrap github --owner=<user> --repository=zeedfai-kubernetes-operator-gitops \
  --branch=main --path=gitops/clusters/staging --personal \
  --components-extra=image-reflector-controller,image-automation-controller
```

Includes Flux 2.9.0 with `image-reflector-controller` and
`image-automation-controller`, plus `ImageRepository`, `ImagePolicy`, and
`ImageUpdateAutomation` for the scorer. Pushing a semver tag like `0.2.1` to
`ghcr.io/nelsudev/zeedfai-scorer` lets Flux update
`gitops/infrastructure/demo/pipeline.yaml` and commit the new image tag back to
`main`. The `ImageRepository` uses the `ghcr-pull` secret in `flux-system`
because the scorer image is private by default.
