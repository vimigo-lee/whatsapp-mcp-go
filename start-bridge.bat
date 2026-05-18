@echo off
cd /d "%~dp0whatsapp-bridge"
set PATH=C:\msys64\ucrt64\bin;%PATH%
set WHATSAPP_API_KEY=178ecbd5c284f327b04b8d35002bd267ca006b01912219646b14be43b6de563b
set WHATSAPP_JWT_SECRET=6b8815fafbc57c0b5c2b5228d0ad1da1643e9007669a6a39425f080250e73184
set IS_POSTGRES=true
set POSTGRES_HOST=localhost
set POSTGRES_PORT=5432
set POSTGRES_USER=postgres
set POSTGRES_PASS=postgres
set PORT=8080
set LOG_LEVEL=info
whatsapp-bridge.exe
