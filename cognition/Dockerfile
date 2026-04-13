FROM node:22-alpine

WORKDIR /app/cognition

COPY package.json package-lock.json ./
RUN npm ci

COPY tsconfig.json ./
COPY src ./src

CMD ["npx", "tsx", "src/server.ts"]
