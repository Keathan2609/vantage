# Terminal image.
#
# Built from the REPOSITORY ROOT because the compose file sets that context:
#   docker build -f apps/web/Dockerfile -t vantage/web .
#
# Next.js `output: "standalone"` produces a server plus only the modules it
# actually imports, so the final image carries no dev dependencies and no
# source tree.

# ------------------------------------------------------------ dependencies
FROM node:22.21-alpine3.22 AS deps

WORKDIR /app

# npm ci from the committed lockfile: reproducible, and it fails rather than
# resolving a different version than the one that was tested.
COPY apps/web/package.json apps/web/package-lock.json ./
RUN npm ci --no-audit --no-fund

# ------------------------------------------------------------------ build
FROM node:22.21-alpine3.22 AS build

WORKDIR /app

COPY --from=deps /app/node_modules ./node_modules
COPY apps/web/ ./

# The API origin is baked into the client bundle at build time, because
# NEXT_PUBLIC_ variables are compiled in. A deployment pointing at a different
# control plane therefore needs its own build — which is the honest behaviour
# rather than pretending it can be changed at runtime.
ARG NEXT_PUBLIC_VANTAGE_API_BASE_URL=http://localhost:8080
ENV NEXT_PUBLIC_VANTAGE_API_BASE_URL=${NEXT_PUBLIC_VANTAGE_API_BASE_URL}
ENV NEXT_TELEMETRY_DISABLED=1
ENV NODE_ENV=production

# A type error or a lint failure fails the image build. Shipping a terminal
# that does not typecheck is how a money-formatting change becomes a runtime
# crash in front of an operator.
RUN npm run typecheck && npm run build

# ------------------------------------------------------------------ runtime
FROM node:22.21-alpine3.22

ENV NODE_ENV=production \
    NEXT_TELEMETRY_DISABLED=1 \
    PORT=3000 \
    HOSTNAME=0.0.0.0

# Apply Alpine's security updates, then remove npm from the runtime image.
# The server is `node server.js`; it never installs a package, and leaving a
# package manager in a production container gives anything that gets a shell a
# way to fetch its next stage. It also takes npm's own bundled dependencies
# out of the vulnerability surface.
RUN apk upgrade --no-cache \
    && rm -rf /usr/local/lib/node_modules/npm /usr/local/bin/npm /usr/local/bin/npx

WORKDIR /app

# node:alpine already ships an unprivileged `node` user (uid 1000).
COPY --from=build --chown=node:node /app/.next/standalone ./
COPY --from=build --chown=node:node /app/.next/static ./.next/static

USER node

EXPOSE 3000

# The root path is enough: it renders the sign-in gate without touching the
# API, so this checks the web tier alone rather than the whole stack.
HEALTHCHECK --interval=15s --timeout=5s --start-period=20s --retries=5 \
    CMD node -e "require('http').get({host:'127.0.0.1',port:3000,path:'/'},r=>process.exit(r.statusCode<400?0:1)).on('error',()=>process.exit(1))"

CMD ["node", "server.js"]
