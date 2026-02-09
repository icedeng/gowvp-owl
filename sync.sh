#!/bin/bash

# 设置颜色输出
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m' # No Color

# 1. 获取最新远程状态
echo -e "${YELLOW}正在从原作者 (origin) 获取更新...${NC}"
git fetch origin

# 2. 确认当前分支
ORIGIN_BRANCH=main
CURRENT_BRANCH=uxl-main
# 切换到分支
echo -e "${YELLOW}切换到分支: ${GREEN}$CURRENT_BRANCH${NC}"
git checkout uxl-main


# 3. 尝试合并 origin 的对应分支
# 假设你想同步的是 main 或 master，这里会自动匹配
echo -e "${YELLOW}正在合并 origin/$ORIGIN_BRANCH 到本地...${NC}"
if git merge origin/$ORIGIN_BRANCH; then
    echo -e "${GREEN}合并成功！${NC}"
else
    echo -e "${RED}发生冲突！请手动解决冲突后再运行脚本。${NC}"
    exit 1
fi

## 4. 推送到你自己的远程 (uxlabs)
#echo -e "${YELLOW}正在推送到你的远程仓库 (uxlabs)...${NC}"
#if git push uxlabs $CURRENT_BRANCH; then
#    echo -e "${GREEN}🎉 所有同步已完成！你的 uxlabs 现在是最新的。${NC}"
#else
#    echo -e "${RED}推送失败，请检查网络或权限。${NC}"
#    exit 1
#fi