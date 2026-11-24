#!/usr/bin/env python3
"""多目标 NDP 应答器 —— 为多个 LXC 容器专用 IPv6 地址应答 Neighbor Solicitation。

关键点：部分服务商（本项目原始场景）只接受「NA 源地址 == 目标地址本身」的应答，
Linux 内核 proxy_ndp 用 link-local 作源地址会被丢弃，因此需要本程序在 AF_PACKET 层手工应答。

用法：
    NDP_IFACE=eth0 NDP_CONF=/etc/ndp-responder.conf python3 ndp-responder.py
配置：NDP_CONF 每行一个 IPv6 地址（支持 # 注释）
重载：kill -HUP <pid> 或 systemctl reload ndp-responder
"""
import socket, struct, time, os, fcntl, signal

IFACE = os.environ.get('NDP_IFACE', 'eth0')
CONF = os.environ.get('NDP_CONF', '/etc/ndp-responder.conf')
NS, NA = 135, 136
S_FLAG, O_FLAG = 0x40000000, 0x20000000
_targets = set()
_socks = []
_reload = False


def load_targets():
    global _targets
    t = set()
    try:
        with open(CONF) as f:
            for line in f:
                line = line.split('#')[0].strip()
                if not line:
                    continue
                try:
                    t.add(socket.inet_pton(socket.AF_INET6, line))
                except Exception:
                    pass
    except FileNotFoundError:
        pass
    _targets = t
    try:
        join_groups(t)
    except Exception as e:
        print("join_groups error:", e, flush=True)
    print("targets loaded: %d -> %s" %
          (len(t), sorted(socket.inet_ntop(socket.AF_INET6, x) for x in t)), flush=True)


def join_groups(targets):
    """为每个目标地址加入 solicited-node 组播组，确保网卡接收对应 NS"""
    global _socks
    for s in _socks:
        try:
            s.close()
        except Exception:
            pass
    _socks = []
    ifindex = socket.if_nametoindex(IFACE)
    for tgt in targets:
        sol = b'\xff\x02' + b'\x00' * 9 + b'\x01\xff' + tgt[-3:]
        try:
            s = socket.socket(socket.AF_INET6, socket.SOCK_DGRAM)
            s.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_JOIN_GROUP,
                         sol + struct.pack('@I', ifindex))
            _socks.append(s)
            print("joined %s" % socket.inet_ntop(socket.AF_INET6, sol), flush=True)
        except Exception as e:
            print("join failed %s: %s" % (socket.inet_ntop(socket.AF_INET6, sol), e), flush=True)


def on_hup(signum, frame):
    global _reload
    _reload = True


def get_mac(ifname):
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    return fcntl.ioctl(s.fileno(), 0x8927, struct.pack('256s', ifname.encode()[:15]))[18:24]


def csum6(src, dst, payload):
    data = src + dst + struct.pack('!IxxxB', len(payload), 58) + payload
    if len(data) % 2:
        data += b'\0'
    s = 0
    for i in range(0, len(data), 2):
        s += (data[i] << 8) + data[i + 1]
    while s >> 16:
        s = (s & 0xffff) + (s >> 16)
    return ~s & 0xffff


def main():
    signal.signal(signal.SIGHUP, on_hup)
    mymac = get_mac(IFACE)
    load_targets()
    sock = socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(0x86dd))
    sock.bind((IFACE, 0))
    sock.settimeout(1.0)
    print("ndp-responder: iface=%s mac=%s conf=%s" % (IFACE, mymac.hex(), CONF), flush=True)
    while True:
        if _reload:
            load_targets()
            globals()['_reload'] = False
        try:
            frame, _ = sock.recvfrom(4096)
        except socket.timeout:
            continue
        except Exception:
            time.sleep(0.5)
            continue
        if len(frame) < 54 or frame[6:12] == mymac or frame[12:14] != b'\x86\xdd':
            continue
        ip6 = frame[14:]
        if len(ip6) < 64 or ip6[6] != 58 or ip6[40] != NS:
            continue
        tgt = ip6[48:64]
        if tgt not in _targets:
            continue
        src_ip, dst_ip = ip6[8:24], ip6[24:40]
        reply_dst = dst_ip if src_ip == b'\x00' * 16 else src_ip
        # 取请求方的链路层地址选项
        peer_mac = None
        off = 64
        while off + 8 <= len(ip6):
            otype, olen = ip6[off], ip6[off + 1] * 8
            if olen == 0:
                break
            if otype == 1 and olen >= 8:
                peer_mac = ip6[off + 2:off + 8]
            off += olen
        if peer_mac is None:
            peer_mac = b'\x33\x33\xff\x00\x00\x10' if reply_dst == dst_ip else b'\x33\x33\x00\x00\x00\x01'
        tllao = struct.pack('!BB', 2, 1) + mymac
        flags = S_FLAG | O_FLAG
        na = struct.pack('!BBHI', NA, 0, 0, flags) + tgt + tllao
        # 注意：源地址必须用目标地址本身
        na = struct.pack('!BBHI', NA, 0, csum6(tgt, reply_dst, na), flags) + tgt + tllao
        hdr = struct.pack('!IHBB', 0x60000000, len(na), 58, 255) + tgt + reply_dst
        try:
            sock.send(peer_mac + mymac + b'\x86\xdd' + hdr + na)
            print("NA -> %s for %s" % (socket.inet_ntop(socket.AF_INET6, reply_dst),
                                        socket.inet_ntop(socket.AF_INET6, tgt)), flush=True)
        except Exception as e:
            print("send error:", e, flush=True)


if __name__ == '__main__':
    main()
