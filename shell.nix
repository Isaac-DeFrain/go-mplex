{ pkgs ? import <nixpkgs> {} }:
pkgs.mkShell {
  buildInputs = with pkgs; [
    go gopls delve go-tools
  ];

  shellHook = '''';
}