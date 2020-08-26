##
# 設定群
##
export GO111MODULE=on
HOST_ADDRESS:=118.27.33.49
#DB_HOST:= ##DBホストアドレス##
#DB_PORT:= ##DBポート番号##
#DB_USER:= ##DBユーザー##
#DB_PASS:= ##DBパス##
#DB_NAME:= ##DBネーム##

DB_HOST=127.0.0.1
DB_PORT=3306
DB_USER=isucari
DB_NAME=isucari
DB_PASS=isucari
export DSTAT_MYSQL_USER=$(DB_USER)
export DSTAT_MYSQL_PWD=$(DB_PASS)
export DSTAT_MYSQL_HOST=$(DB_HOST)

MYSQL_CMD:=mysql -h$(DB_HOST) -P$(DB_PORT) -u$(DB_USER) -p$(DB_PASS) $(DB_NAME)

NGX_LOG:=/tmp/access.log
MYSQL_LOG:=/tmp/slow-query.log

KATARIBE_CFG:=~/kataribe.toml

SLACKCAT:=slackcat --tee --channel ##チャンネル名##
SLACKRAW:=slackcat --channel ##チャンネル名##

# PPROF:=go tool pprof -seconds=120  -png -output pprof.png http://localhost:6060/debug/pprof/profile
# PPROF:=go tool pprof -png -output pprof.png -http=118.27.33.49:6000 -no_browser http://localhost:6060/debug/fgprof?seconds=120
PPROF:=go tool pprof -output profile.pb.gz -seconds=120 http://localhost:6060/debug/fgprof

PROJECT_ROOT:=~/isucari/webapp/go ##プロジェクトルートディレクトリ##
BUILD_DIR:=~/isucari/webapp/go ##バイナリ生成先##
BIN_NAME:=isucari ##生成バイナリ名##

CURL_OPTIONS:=-o /dev/null -s -w "%{http_code}\n"

APP_SERVICE:=isucari.golang ##systemdサービス名##
REPOSITORY_URL:=https://github.com/FujishigeTemma/isucon9-qualify ##リポジトリのURL##

TAG:= 0
COMMIT_HASH:=0

##
# コマンド群
##
all: build

.PHONY: push
push:
	@git add .
	@git commit -m "changes from server"
	@git push

.PHONY: update
update: pull build restart curl

pull:
	@git pull
	@cd $(PROJECT_ROOT) && \
		go mod tidy && \
		go mod download

.PHONY: build
build:
	@cd $(BUILD_DIR) && \
		make
	#@go build -o $(BIN_NAME)

.PHONY: restart
restart:
	@sudo systemctl restart $(APP_SERVICE)

.PHONY: bench
bench: stash-log log

.PHONY: stash-log
stash-log:
	@$(eval when := $(shell date "+%s"))
	@mkdir -p ~/logs/$(when)
	@if [ -f $(NGX_LOG) ]; then \
		sudo mv -f $(NGX_LOG) ~/logs/$(when)/ ; \
	fi
	@if [ -f $(MYSQL_LOG) ]; then \
		sudo mv -f $(MYSQL_LOG) ~/logs/$(when)/ ; \
	fi
	@sudo systemctl restart nginx
	@sudo systemctl restart mysql

.PHONY: curl
curl:
	@curl localhost $(CURL_OPTIONS)

.PHONY: status
status:
	@sudo systemctl status $(APP_SERVICE)

.PHONY: rollback
rollback: reset build restart curl

reset:
ifeq ($(COMMIT_HASH),0)
	@echo "Please set variable: COMMIT_HASH={{commit_hash}}"
else
	@git reset --hard $(COMMIT_HASH)
endif

.PHONY: log
log:
	@sudo journalctl -u $(APP_SERVICE) -n10 -f

.PHONY: tag
tag:
ifeq ($(TAG),0)
	@echo "Please set variable: TAG={{bench_score}}"
else
	@git tag $(TAG)
	@git push origin $(TAG)
endif

.PHONY: dbstat
dbstat:
	dstat -T --mysql5-cmds --mysql5-io --mysql5-keys

.PHONY: analytics
analytics: kataru dumpslow digestslow pprof

.PHONY: kataru
kataru:
	@sudo cat $(NGX_LOG) | kataribe -f ~/kataribe.toml | $(SLACKCAT) kataribe

.PHONY: pprof
pprof:
	@$(PPROF)
	@go tool pprof -png -output pprof.png profile.pb.gz
	@$(SLACKRAW) pprof -n pprof.png ./pprof.png
	@go tool pprof -http=$(HOST_ADDRESS):6000 -no_browser profile.pb.gz

.PHONY: dumpslow
dumpslow:
	@sudo mysqldumpslow -s t -t 10 $(MYSQL_LOG) | $(SLACKCAT) slowquery

.PHONY: digestslow
digestslow:
	@sudo pt-query-digest $(MYSQL_LOG) | $(SLACKCAT) slowquery

.PHONY: slow-on
slow-on:
	@sudo mysql -e "set global slow_query_log_file = '$(MYSQL_LOG)'; set global long_query_time = 0; set global slow_query_log = ON;"

.PHONY: slow-off
slow-off:
	@sudo mysql -e "set global slow_query_log = OFF;"

.PHONY: prune
prune: stash-log slow-off pull build curl
	@echo -e '\e[35mpprofをコードから取り除いたことを確認してください。\nNginxのログ出力を停止したことを確認してください。\e[0m\n'

##
# 諸々のインストールと設定
##
.PHONY: setup
setup: apt install-tools ssh-key git-init

apt:
	@sudo apt update
	@sudo apt upgrade -y

# バックアップするディレクトリは必要に応じて変更
backup:
	@tar -czvpf ~/backup.tar.gz -C ~/ .
	@echo -e '\e[35mscp {{ユーザー名}}@{{IP address}}:~/backup.tar.gz ~/Downloads/ を手元で実行してください。\nリカバリは他のサーバーからでも可能です。\e[0m\n'

install-go:
	@wget https://golang.org/dl/go1.14.6.linux-amd64.tar.gz -O golang.tar.gz
	@tar -xzf golang.tar.gz
	@rm -rf golang.tar.gz
	@sudo mv go /usr/local/
	@sudo chmod +x /usr/local/go/bin/go
	@sudo ln -fs /usr/local/go/bin/go /usr/bin/go

install-tools: install-kataribe install-myprofiler install-slackcat
	@sudo apt install -y htop dstat percona-toolkit

# Nginxのログ解析
install-kataribe:
	@sudo apt install -y unzip
	@wget https://github.com/matsuu/kataribe/releases/download/v0.4.1/kataribe-v0.4.1_linux_amd64.zip -O kataribe.zip
	@sudo unzip -o -d /usr/local/bin/ kataribe.zip
	@sudo chmod +x /usr/local/bin/kataribe
	@rm kataribe.zip
	@kataribe -generate
	@echo -e '\e[35mgenarated $(pwd)/kataribe.toml\nsudo nano /etc/nginx/nginx.conf 参考:https://github.com/matsuu/kataribe#Nginx\n##\n# Logging Settings\n##\n以下に追加してください。\n出力先には$(NGX_LOG)を指定してください。\nsudo nginx -t\nsudo systemctl reload nginx\e[0m\n'

install-myprofiler:
	@wget https://github.com/KLab/myprofiler/releases/download/0.2/myprofiler.linux_amd64.tar.gz -O myprofiler.tar.gz
	@tar -xzf myprofiler.tar.gz
	@rm myprofiler.tar.gz
	@sudo mv myprofiler /usr/local/bin/
	@sudo chmod +x /usr/local/bin/myprofiler
	@echo -e '\e[35mスロークエリの出力時には slow-on を実行してください。\e[0m\n'

install-slackcat:
	@wget https://github.com/bcicen/slackcat/releases/download/v1.6/slackcat-1.6-linux-amd64 -O slackcat
	@sudo mv slackcat /usr/local/bin/
	@sudo chmod +x /usr/local/bin/slackcat
	@slackcat --configure

ssh-key:
	@mkdir -p ~/.ssh
	@echo "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQD1g/cXhxb6VDjqgIfEeUwjSR6qf+n30Z2Cf4fhSX1ZF1x+Glqb/NsaRhEYqiG4jaLMXGZpXddQaUHn1eXdgM06BOVtDlDN2PeN5o6COfBnNR64Aa9+wYbEgmIXNW6ZBb9zKM2+n4rJE5Ihobqu68nJwUdmZv3BLeoP6Lr6Ze0N4PvCsLEwOsw9KqJuNybrAcGM/6DJuP7bZXTrQJp1Qwwxqdmk4dOEeWIdacQrq5W5nO4n2xXkmAAQ+Q78V/rwpq4SXdcxNzo3alzcZkOuvyZPXSY8xar7Vvb8ERXa6oFaCXkRdWr9VXXWmGpFDP0iXZE3/2yn3VeZEWRwvThoIFtd ryoha@ryoha-pc" >> ~/.ssh/authorized_keys
	@echo "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGVy1KogqLG7pPTcsm5zhC5RjddrAOfX7rHGK4K8y4s7 green@DESKTOP-V1DT07E" >> ~/.ssh/authorized_keys
	@echo "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKGvCNw4WJiTg327zw9AYchInFHxzlwBgzkm12fRIGAT tenma.x0@gmail.com" >> ~/.ssh/authorized_keys

git-init:
	@git config --global user.email "tenma.x0@gmail.com"
	@git config --global user.name "FujishigeTemma"
	@ssh-keygen -t ed25519 -q
	@cat ~/.ssh/id_ed25519.pub
	@echo -e '\e[35mhttps://github.com/settings/keys に追加してください。\e[0m\n'
	@read -p $'\e[35mPress enter to continue.\e[0m\n'
	@git add .
	@git commit -m "init"
	@git remote add origin $(REPOSITORY_URL)
	@git push -u origin master
