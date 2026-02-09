# UXL 说明

## 修改记录

- 0206: Makefile中增加构建 ARM64 镜像的命令
- 0209: 通道未设置时的默认录像模式在配置文件中配置

## 多 Git 源

```shell
# 增加 UXLabs 的远程地址
git remote add uxlabs ssh://git@git.uxlabs.me:2222/labs/gowvp-owl.git

# 推送到 uxlabs：
git push uxlabs main

# 抓取源作者的更新
git fetch origin
# 合并到修改的本地分支
git checkout uxl-main
git merge origin/main
```
