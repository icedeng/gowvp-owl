FROM node:22-alpine AS admin-ui-builder

WORKDIR /src/admin-ui
RUN corepack enable
COPY admin-ui/package.json admin-ui/pnpm-lock.yaml admin-ui/pnpm-workspace.yaml ./
RUN pnpm install --frozen-lockfile
COPY admin-ui/ ./
RUN pnpm build

FROM alpine:latest

ARG TARGETARCH

ENV TZ=Asia/Shanghai

RUN apk --no-cache add ca-certificates \
	tzdata

WORKDIR /app

ADD ./build/linux_${TARGETARCH}/bin ./
COPY --from=admin-ui-builder /src/admin-ui/dist /app/www

LABEL Name=gowvp Version=0.0.1

EXPOSE 15123

CMD [ "./bin" ]
