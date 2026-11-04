FROM mcr.microsoft.com/dotnet/sdk:9.0 AS base
WORKDIR /app
COPY h.csproj ./

FROM base AS probe
RUN echo "CACHEDIR=$(dotnet nuget locals global-packages --list | sed 's/^global-packages: //')"

FROM base AS install
ARG BUST=0
RUN dotnet restore
