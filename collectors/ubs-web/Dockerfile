# Headless-browser collector: build on the shared Playwright base
# (collectorkit pre-installed; the Playwright image tag is pinned there,
# and `make update` moves it and the `playwright` pin in requirements.txt
# together).
ARG WEALTHDB_IMAGE_TAG=latest
FROM wealthdb/base-playwright:${WEALTHDB_IMAGE_TAG}

# Install collector-specific deps first so editing scripts doesn't bust
# the pip cache layer.
COPY requirements.txt /app/
RUN pip install -r requirements.txt

# Copy scripts last. Listed explicitly so stray host artefacts (.venv,
# real-credential JSONs, recorded Playwright traces) never reach the
# image. Every module the verbs import must be listed:
# shared/collectorkit/tests/test_collector_images.py checks it.
COPY login.py download.py load.py prune.py explore.py cards.py card_parsers.py landmarks.py pdf_parsers.py \
     pdf_common.py soa_positions.py soa_transaction_list.py account_statement.py maturity_notice.py \
     payment_advice.py securities_advice.py quarterly_report.py rates.py rate_parsers.py /app/
COPY migrations /app/migrations
COPY entrypoint.sh /app/
RUN chmod +x /app/entrypoint.sh

# Default user is root. The wrapper runs the container with
# `--user $(id -u):$(id -g)` so files written to mounted volumes
# have host-correct ownership; Chromium's user-namespace sandbox is
# disabled at launch time in code with `--no-sandbox` instead. This
# is acceptable in a single-tenant, single-trusted-origin scenario
# (the collector loads only ubs.com).

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["help"]
