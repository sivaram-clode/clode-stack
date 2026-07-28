-- Bootstraps every per-service database inside the shared Postgres.
-- Executed once by the official postgres image on first startup
-- (via /docker-entrypoint-initdb.d/).

CREATE DATABASE raksha;
CREATE DATABASE jumbo;
CREATE DATABASE brahmi;
CREATE DATABASE "pool-manager";
CREATE DATABASE chil_new;
CREATE DATABASE chaching;
CREATE DATABASE skills_registry;
CREATE DATABASE narnia;
CREATE DATABASE gitana;
CREATE DATABASE toolkit_proxy;
CREATE DATABASE intervix;
CREATE DATABASE vova;
CREATE DATABASE ikki;
CREATE DATABASE notify;
