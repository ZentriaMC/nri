{ pkgs ? import <nixpkgs> { }, ... }:
pkgs.mkShell rec {
  nativeBuildInputs = with pkgs; [ go golangci-lint gopls ];
}
