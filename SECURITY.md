# Security Policy

## Reporting a vulnerability

**Please do not report security vulnerabilities through public GitHub issues,
pull requests, or discussions.**

Instead, use GitHub's private vulnerability reporting:

1. Go to the repository's **Security** tab.
2. Click **Report a vulnerability** (this opens a private advisory visible only
   to the maintainers).
3. Include a description of the issue, the affected component (Terraform module,
   Helm chart, CI image, or the `llz` CLI), reproduction steps, and the impact
   you've assessed.

Maintainers may also be reached privately if you cannot use GitHub advisories;
add a contact address here for your fork/deployment.

## What to expect

- We aim to acknowledge a report within **5 business days**.
- We will work with you to confirm the issue, determine its severity, and agree
  on a disclosure timeline. Please give us reasonable time to ship a fix before
  any public disclosure.
- Credit is given to reporters who wish to be named, once a fix is released.

## Scope

This project is a *landing zone template*: it ships building blocks (Terraform
modules, Helm charts, CI images, the `llz` CLI) that an adopting team deploys
into their own Linode/Akamai account. Reports about the **published artifacts and
their secure-by-default posture** are in scope. Misconfigurations in a specific
adopter's deployment are that adopter's responsibility, though we welcome reports
of defaults that are unsafe out of the box.
