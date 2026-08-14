FROM node:22.23.1-bookworm-slim AS build

WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
RUN npm run check && npm test && npm run build && npm prune --omit=dev

FROM node:22.23.1-bookworm-slim

ENV NODE_ENV=production \
    PIE_WEB_CHAT_HOST=0.0.0.0 \
    PIE_WEB_CHAT_PORT=4175

WORKDIR /app
COPY --from=build --chown=node:node /app /app

USER node
EXPOSE 4175

CMD ["npm", "start"]
