# wealthdb/metabase — Metabase built from the official JAR on a glibc
# base (Ubuntu/temurin), NOT the official Alpine image.
#
# Why from-JAR: the community DuckDB driver bundles a glibc-built JDBC
# native (libduckdb_java.so → needs libstdc++.so.6 + glibc). The
# official Metabase image is Alpine/musl, where that native hard-crashes
# the JVM (even with gcompat). A glibc base — which already ships
# libstdc++.so.6 — lets the native load cleanly. See DESIGN.md.
#
# Pins: Metabase v0.59.15 + community metabase_duckdb_driver 1.5.3.0
# (bundles DuckDB 1.5.3 → matches the gold DB's 1.5.x storage format).
FROM eclipse-temurin:21-jre-noble

# Metabase needs fontconfig + a font for headless AWT rendering. glibc +
# libstdc++6 (for the DuckDB native) are already present in the base.
RUN apt-get update && apt-get install -y --no-install-recommends \
      fontconfig fonts-dejavu-core && \
    rm -rf /var/lib/apt/lists/*

ARG MB_VERSION=v0.59.15
ADD https://downloads.metabase.com/${MB_VERSION}/metabase.jar /app/metabase.jar

ARG DRIVER_URL=https://github.com/motherduckdb/metabase_duckdb_driver/releases/download/1.5.3.0/duckdb.metabase-driver.jar
ARG DRIVER_SHA256=4a94e2e236ed0fa6215e8db98798ab4246a4a7b743ac472dc8f946542af6538e
# --chmod=0644 so Metabase can read the JAR (ADD-from-URL is 0600 by
# default); --checksum verifies the download against the pin.
ADD --chmod=0644 --checksum=sha256:${DRIVER_SHA256} ${DRIVER_URL} /plugins/duckdb.metabase-driver.jar

ENV MB_PLUGINS_DIR=/plugins
WORKDIR /app
EXPOSE 3000
CMD ["java", "-jar", "/app/metabase.jar"]
