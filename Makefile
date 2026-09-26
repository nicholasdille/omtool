M                   = $(shell printf "\033[34;1m▶\033[0m")

GIT ?= git
ifeq ($(DEBUG_RELEASE), true)
	GIT := echo git
endif

.PHONY:
tag--%: ; $(info $(M) Creating tag v$*...)
	@if git show-ref --tags refs/tags/v$* >/dev/null 2>&1; then \
		echo "Tag v$* already exists"; \
		exit 1; \
	fi
	@$(GIT) tag -a -m $* v$*

.PHONY:
push--%: ; $(info $(M) Pushing tag v$*...)
	@$(GIT) push origin v$*

.PHONY:
retag--%: ; $(info $(M) Creating tag v$*...)
	@$(GIT) tag -a -m $* -f v$*

.PHONY:
repush--%: ; $(info $(M) Pushing tag v$*...)
	@$(GIT) push origin v$* -f

.PHONY:
push: ; $(info $(M) Pushing tags...)
	@$(GIT) push origin --tags
