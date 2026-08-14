import { afterEach, describe, expect, it, vi } from "vitest";
import {
  createManagedDeviceSession,
  createManagedGrant,
  createManagedHostSession,
  issueManagedParticipantCredential,
  listManagedResources,
  revokeManagedGrant,
  type ControlIdentity,
  type ManagedDeviceSummary,
  type ManagedSessionSummary,
} from "./control-plane";

describe("createManagedHostSession", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("registers the device, creates a scoped session, then requests the host credential", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        new Response(JSON.stringify({ id: "dev-1" }), { status: 201 }),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            id: "ses-1",
            ownerUserId: "user-1",
            deviceId: "dev-1",
          }),
          {
            status: 201,
          },
        ),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({ token: "jwt", expiresAt: "2030-01-01T00:00:00Z" }),
          {
            status: 201,
          },
        ),
      );
    vi.stubGlobal("fetch", fetchMock);

    const result = await createManagedHostSession(
      "https://control.cookai.dev/path",
      "pat",
      {
        deviceId: "dev-1",
        deviceName: "Mac",
        sessionName: "Work",
        executionTarget: "local",
        accessMode: "shared",
        transportMode: "auto",
      },
    );

    expect(result).toMatchObject({
      sessionId: "ses-1",
      deviceId: "dev-1",
      token: "jwt",
    });
    expect(fetchMock.mock.calls.map((call) => call[0])).toEqual([
      "https://control.cookai.dev/v1/control/devices/register",
      "https://control.cookai.dev/v1/control/sessions",
      "https://control.cookai.dev/v1/control/sessions/ses-1/credential",
    ]);
    for (const call of fetchMock.mock.calls) {
      expect((call[1] as RequestInit).headers).toMatchObject({
        Authorization: "Bearer pat",
      });
    }
    expect(
      JSON.parse(String((fetchMock.mock.calls[0][1] as RequestInit).body)),
    ).toMatchObject({
      id: "dev-1",
      kind: "local",
    });
    expect(
      JSON.parse(String((fetchMock.mock.calls[1][1] as RequestInit).body)),
    ).toMatchObject({
      deviceId: "dev-1",
      executionTarget: "local",
      accessMode: "shared",
      transportMode: "auto",
    });
  });

  it("uses the canonical docker device kind for managed containers", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        new Response(JSON.stringify({ id: "dev-docker" }), { status: 201 }),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            id: "ses-docker",
            ownerUserId: "user-1",
            deviceId: "dev-docker",
          }),
          { status: 201 },
        ),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({ token: "jwt", expiresAt: 1_900_000_000 }),
          {
            status: 201,
          },
        ),
      );
    vi.stubGlobal("fetch", fetchMock);

    await createManagedHostSession("https://control.cookai.dev", "pat", {
      deviceId: "dev-docker",
      deviceName: "Workspace",
      executionTarget: "docker",
      accessMode: "private",
      transportMode: "relay",
    });

    expect(
      JSON.parse(String((fetchMock.mock.calls[0][1] as RequestInit).body)),
    ).toMatchObject({
      kind: "docker",
    });
  });

  it("sends versioned resource context and trusts the credential response instead of decoding JWT", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            userId: "user-1",
            organizationId: "workspace-1",
            roles: [],
            admin: false,
          }),
          { status: 200 },
        ),
      )
      .mockResolvedValueOnce(
        new Response(JSON.stringify({ id: "dev-1" }), { status: 201 }),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            id: "ses-1",
            ownerUserId: "user-1",
            deviceId: "dev-1",
          }),
          { status: 201 },
        ),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            token: "opaque-token",
            room: "r_v2_opaque-room",
            relayUrl: "https://desktop-relay.example",
            executionTarget: "local",
            expiresAt: 1_900_000_000,
          }),
          { status: 201 },
        ),
      );
    vi.stubGlobal("fetch", fetchMock);

    const result = await createManagedHostSession(
      "https://control.cookai.dev",
      "pat",
      {
        deviceId: "dev-1",
        deviceName: "Mac",
        executionTarget: "local",
        accessMode: "private",
        transportMode: "relay",
        applicationId: "pie-relay-desktop",
        poolId: "pie-relay-default",
      },
    );

    expect(result.room).toBe("r_v2_opaque-room");
    expect(result.relayUrl).toBe("https://desktop-relay.example");
    expect(fetchMock.mock.calls.map((call) => call[0])).toEqual([
      "https://control.cookai.dev/v1/control/me",
      "https://control.cookai.dev/v1/control/devices/register",
      "https://control.cookai.dev/v1/control/sessions",
      "https://control.cookai.dev/v1/control/sessions/ses-1/credential",
    ]);
    expect(
      JSON.parse(String((fetchMock.mock.calls[2][1] as RequestInit).body)),
    ).toMatchObject({
      applicationId: "pie-relay-desktop",
      poolId: "pie-relay-default",
      tenantId: "workspace-1",
      resourceType: "device",
      resourceId: "dev-1",
      protocol: "terminal",
    });
  });

  it("creates a session on the selected device without re-registering it", async () => {
    const docker: ManagedDeviceSummary = {
      id: "docker-user-1",
      ownerUserId: "user-1",
      name: "사용자 작업 공간",
      kind: "docker",
      observedState: "online",
      relayRegistered: true,
    };
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          id: "ses-selected",
          ownerUserId: "user-1",
          deviceId: docker.id,
          name: "분리 작업",
          executionTarget: "docker",
          accessMode: "shared",
          transportMode: "relay",
          status: "starting",
        }),
        { status: 201 },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);

    const created = await createManagedDeviceSession(
      "http://127.0.0.1:19090/path",
      "pat",
      docker,
      { name: "  분리 작업  ", accessMode: "shared" },
    );

    expect(created.deviceId).toBe(docker.id);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls[0][0]).toBe(
      "http://127.0.0.1:19090/v1/control/sessions",
    );
    expect(
      JSON.parse(String((fetchMock.mock.calls[0][1] as RequestInit).body)),
    ).toEqual({
      deviceId: docker.id,
      name: "분리 작업",
      executionTarget: "docker",
      accessMode: "shared",
      transportMode: "relay",
      status: "starting",
    });
  });

  it("surfaces authentication failure without leaking the PAT", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(new Response("no", { status: 401 })),
    );
    await expect(
      createManagedHostSession("http://127.0.0.1:18080", "top-secret", {
        deviceId: "dev-1",
        deviceName: "Mac",
        executionTarget: "local",
        accessMode: "private",
        transportMode: "relay",
      }),
    ).rejects.toThrow("PAT 인증에 실패했습니다");
  });

  it("loads own and shared Control Plane resources with the PAT on every request", async () => {
    const responses = [
      { userId: "guest", roles: [], admin: false },
      [{ id: "dev-1", ownerUserId: "owner", name: "Linux", kind: "local" }],
      [
        {
          id: "ses-1",
          ownerUserId: "owner",
          deviceId: "dev-1",
          executionTarget: "local",
          accessMode: "shared",
          transportMode: "relay",
          status: "active",
        },
      ],
      [
        {
          id: "grant-1",
          ownerUserId: "owner",
          subjectUserId: "guest",
          targetDeviceId: "dev-1",
          sessionId: "ses-1",
          access: "view",
          expiresAt: "2030-01-01T00:00:00Z",
        },
      ],
    ];
    const fetchMock = vi.fn((_: string, init?: RequestInit) =>
      Promise.resolve(
        new Response(JSON.stringify(responses.shift()), {
          status: 200,
          headers: {
            "x-auth": String(
              (init?.headers as Record<string, string>)?.Authorization,
            ),
          },
        }),
      ),
    );
    vi.stubGlobal("fetch", fetchMock);

    const result = await listManagedResources(
      "https://control.cookai.dev",
      "pat-shared",
    );
    expect(result.identity.userId).toBe("guest");
    expect(result.sessions.map((value) => value.id)).toEqual(["ses-1"]);
    expect(result.grants[0].access).toBe("view");
    for (const call of fetchMock.mock.calls) {
      expect((call[1] as RequestInit).headers).toMatchObject({
        Authorization: "Bearer pat-shared",
      });
    }
  });

  it("mints participant view credentials for a shared session and host credentials for the owner", async () => {
    const session: ManagedSessionSummary = {
      id: "ses-1",
      ownerUserId: "owner",
      deviceId: "dev-1",
      executionTarget: "local",
      accessMode: "shared",
      transportMode: "relay",
      status: "active",
    };
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          token: "jwt",
          room: "owner",
          access: "view",
          relayUrl: "https://relay.cookai.dev",
        }),
        {
          status: 201,
        },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);
    const guest: ControlIdentity = { userId: "guest", roles: [], admin: false };

    const participant = await issueManagedParticipantCredential(
      "https://control.cookai.dev",
      "pat",
      session,
      guest,
      "view",
    );
    expect(participant).toEqual({
      token: "jwt",
      room: "owner",
      asHost: false,
      access: "view",
      relayUrl: "https://relay.cookai.dev",
      executionTarget: "local",
    });
    expect(
      JSON.parse(String((fetchMock.mock.calls[0][1] as RequestInit).body)),
    ).toMatchObject({
      role: "participant",
      access: "view",
    });

    fetchMock.mockClear();
    fetchMock.mockResolvedValue(
      new Response(
        JSON.stringify({ token: "host-jwt", room: "owner", access: "control" }),
        {
          status: 201,
        },
      ),
    );
    const owner: ControlIdentity = { userId: "owner", roles: [], admin: false };
    const host = await issueManagedParticipantCredential(
      "https://control.cookai.dev",
      "pat",
      session,
      owner,
      "view",
    );
    expect(host.asHost).toBe(true);
    expect(
      JSON.parse(String((fetchMock.mock.calls[0][1] as RequestInit).body)),
    ).toMatchObject({
      role: "host",
      access: "control",
    });
  });

  it("creates and revokes a session-scoped sharing grant", async () => {
    const session: ManagedSessionSummary = {
      id: "ses-1",
      ownerUserId: "owner",
      deviceId: "dev-1",
      executionTarget: "local",
      accessMode: "shared",
      transportMode: "relay",
      status: "active",
    };
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          id: "grant-1",
          ownerUserId: "owner",
          subjectUserId: "guest",
          targetDeviceId: "dev-1",
          sessionId: "ses-1",
          access: "control",
          expiresAt: "2030-01-01T00:00:00Z",
        }),
        { status: 201 },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);

    await createManagedGrant(
      "https://control.cookai.dev",
      "pat",
      session,
      " guest ",
      "control",
    );
    expect(fetchMock.mock.calls[0][0]).toBe(
      "https://control.cookai.dev/v1/control/grants",
    );
    expect(
      JSON.parse(String((fetchMock.mock.calls[0][1] as RequestInit).body)),
    ).toMatchObject({
      subjectUserId: "guest",
      targetDeviceId: "dev-1",
      sessionId: "ses-1",
      access: "control",
    });

    fetchMock.mockClear();
    fetchMock.mockResolvedValue(
      new Response(JSON.stringify({ id: "grant-1", revokedAt: "2030-01-01" }), {
        status: 200,
      }),
    );
    await revokeManagedGrant("https://control.cookai.dev", "pat", "grant-1");
    expect(fetchMock.mock.calls[0][0]).toBe(
      "https://control.cookai.dev/v1/control/grants/grant-1/revoke",
    );
  });
});
