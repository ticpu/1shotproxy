NAME=1shotproxy
DOCKER=podman
DISTRO=stretch buster bullseye
FILES=main.go go.mod go.sum

.PHONY: all clean

all: $(foreach distro,$(DISTRO),$(NAME)-$(distro))

clean:
	rm -f $(NAME)-*

define make_target =
$$(NAME)-$(1): $$(FILES)
	$$(DOCKER) run --pull always -q -v $$(PWD):/app -w /app --rm docker.io/library/golang:$(1) go build -o $$@

endef

$(eval $(foreach distro,$(DISTRO),$(call make_target,$(distro))))