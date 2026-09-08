# 可疑取现协同止付事件服务 scaffold

This repository is an intentionally incomplete starting point for a pure backend service. It contains input contracts, deterministic fixtures, and a Docker-based scaffold validator; no requested business API is implemented.

Business theme: 银行警方联手拦截被骗现金
Theme source: https://www.chinanews.com/scroll-news/news1.html
Required stack: Go 1.25, PostgreSQL, Docker Compose

Validate the baseline inputs with:

```sh
docker compose run --rm --no-deps scaffold-check
```

The implementation must preserve the contracts and fixtures, add the service and its automated tests, and provide a repeatable Docker-based black-box self-test. External production systems must not be used.
